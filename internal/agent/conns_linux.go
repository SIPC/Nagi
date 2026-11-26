//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/project"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"golang.org/x/sys/unix"
)

// connsSupported：网络连接视图只在 Linux 上有（sock_diag、/proc）。
const connsSupported = true

// containersTTL 是容器列表（ID → 名称、项目）的缓存时长：每轮都问 Docker 太频繁，容器几秒内变化也不要紧。
const containersTTL = 10 * time.Second

// connsState 是采样之间留着的东西：容器列表的缓存。只在采样的 goroutine 里用。
type connsState struct {
	mu         sync.Mutex
	containers map[string]containerOwner
	at         time.Time
}

type containerOwner struct {
	name, project, projectName, container, service string
}

func newAgentConns(a *agent) *connWatch {
	st := &connsState{}
	return newConnWatch(func(cmdline bool) (connSample, error) { return a.sampleConns(st, cmdline) })
}

// procInfo 是一个进程：名称、命令行、所在的网络命名空间和 cgroup 指向的容器或单元。
type procInfo struct {
	pid               int
	comm, cmdline     string
	containerID, unit string
}

// sampleConns 做一轮采样：扫 /proc 找出每个网络命名空间和每个 socket 的持有进程，再到每个命名空间里 dump 连接。
func (a *agent) sampleConns(st *connsState, cmdline bool) (connSample, error) {
	start := time.Now()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return connSample{}, err
	}
	owners := map[uint32]*procInfo{} // socket inode → 进程
	nsPID := map[string]int{}        // 网络命名空间 → 其中一个进程
	nsOrder := []string{}            // 本机的命名空间排第一
	self, _ := os.Readlink("/proc/self/ns/net")
	if self != "" {
		nsPID[self] = os.Getpid()
		nsOrder = append(nsOrder, self)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := "/proc/" + e.Name()
		ns, err := os.Readlink(dir + "/ns/net")
		if err != nil {
			continue // 进程退出了，或者是内核线程
		}
		if _, ok := nsPID[ns]; !ok {
			nsPID[ns] = pid
			nsOrder = append(nsOrder, ns)
		}
		fds, err := os.ReadDir(dir + "/fd")
		if err != nil {
			continue
		}
		var p *procInfo
		for _, fd := range fds {
			link, err := os.Readlink(dir + "/fd/" + fd.Name())
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 32)
			if err != nil {
				continue
			}
			if _, ok := owners[uint32(inode)]; ok {
				continue // fork 出来共用的 socket：记第一个（通常是父进程）
			}
			if p == nil {
				p = readProc(pid, cmdline)
			}
			owners[uint32(inode)] = p
		}
	}
	containers := st.containerOwners(a)
	out := connSample{}
	for _, ns := range nsOrder {
		sockets, err := dumpNamespace(nsPID[ns], ns == self)
		if err != nil {
			out.nsError++
			continue
		}
		for _, sc := range connsOf(ns, sockets) {
			if p := owners[sc.inode]; p != nil && sc.inode != 0 {
				fillOwner(sc.c, p, containers)
			}
			out.conns = append(out.conns, sc)
		}
	}
	out.took = time.Since(start)
	return out, nil
}

func readProc(pid int, cmdline bool) *procInfo {
	dir := "/proc/" + strconv.Itoa(pid)
	p := &procInfo{pid: pid}
	if b, err := os.ReadFile(dir + "/comm"); err == nil {
		p.comm = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(dir + "/cgroup"); err == nil {
		p.containerID, p.unit = cgroupOwner(string(b))
	}
	if cmdline {
		if b, err := os.ReadFile(dir + "/cmdline"); err == nil {
			p.cmdline = cleanCmdline(b)
		}
	}
	return p
}

func fillOwner(c *nagiv1.NodeConnection, p *procInfo, containers map[string]containerOwner) {
	c.Pid, c.Process, c.Cmdline, c.Unit = uint32(p.pid), p.comm, p.cmdline, p.unit
	if p.containerID == "" {
		return
	}
	o, ok := containers[p.containerID]
	if !ok {
		c.DockerName = p.containerID[:12]
		return
	}
	c.DockerName, c.ProjectId, c.ProjectName, c.Container, c.Service = o.name, o.project, o.projectName, o.container, o.service
}

// containerOwners 返回容器 ID → 容器名和 Nagi 项目，缓存 containersTTL。Docker 不可用时是空的。
func (st *connsState) containerOwners(a *agent) map[string]containerOwner {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.containers != nil && time.Since(st.at) < containersTTL {
		return st.containers
	}
	m := map[string]containerOwner{}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	if a.docker != nil {
		names := map[string]string{}
		if a.projects != nil {
			for _, c := range a.projects.List(ctx) {
				names[c.Spec.GetProjectId()] = c.Spec.GetProjectName()
			}
		}
		if list, err := a.docker.ImageContainers(ctx); err == nil {
			for _, c := range list {
				o := containerOwner{name: c.Name}
				if pid := c.Labels[project.LabelProject]; pid != "" {
					o.project, o.projectName, o.container = pid, names[pid], c.Labels["nagi.container"]
					o.service = c.Labels["com.docker.compose.service"]
				}
				m[c.ID] = o
			}
		}
	}
	st.containers, st.at = m, time.Now()
	return m
}

// dumpNamespace 在进程 pid 所在的网络命名空间里 dump TCP 和 UDP（IPv4、IPv6）。own 为 true 时就是本机的，不用切换。
// 切换命名空间只影响当前线程：在锁住的线程里切进去建好 netlink socket 再切回来，socket 留在那个命名空间里。
// 切不回来时不解锁线程，goroutine 结束时 Go 会丢掉这个线程。
func dumpNamespace(pid int, own bool) ([]diagSocket, error) {
	fd, err := netlinkIn(pid, own)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var out []diagSocket
	seq := uint32(0)
	for _, fam := range []uint8{afInet, afInet6} {
		for _, proto := range []uint8{ipprotoTCP, ipprotoUDP} {
			seq++
			got, err := dump(fd, fam, proto, seq)
			if err != nil {
				return nil, err
			}
			out = append(out, got...)
		}
	}
	return out, nil
}

func netlinkIn(pid int, own bool) (int, error) {
	open := func() (int, error) {
		fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
		if err != nil {
			return -1, err
		}
		tv := unix.Timeval{Sec: 2}
		_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
		return fd, nil
	}
	if own {
		return open()
	}
	target, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "ns", "net"))
	if err != nil {
		return -1, err
	}
	defer target.Close()
	type result struct {
		fd  int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			ch <- result{-1, err}
			return
		}
		defer orig.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			ch <- result{-1, err}
			return
		}
		fd, err := open()
		if unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET) == nil {
			runtime.UnlockOSThread()
		}
		ch <- result{fd, err}
	}()
	r := <-ch
	return r.fd, r.err
}

func dump(fd int, family, protocol uint8, seq uint32) ([]diagSocket, error) {
	if err := unix.Sendto(fd, diagRequest(family, protocol, seq), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}
	buf := make([]byte, 64<<10)
	var out []diagSocket
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, err
		}
		got, done, err := parseDiag(buf[:n], protocol)
		out = append(out, got...)
		if err != nil || done {
			return out, err
		}
	}
}
