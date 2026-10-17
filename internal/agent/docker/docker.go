// Package docker 通过 Docker Engine API 读取容器状态、日志和事件。
// 部署本身走 docker compose 命令（见 compose 包），这里只做读取。
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
	labelMember  = "nagi.member"
)

// ComposeLabel 是选出某个 compose 项目所有容器的标签条件。
func ComposeLabel(composeProject string) string { return labelProject + "=" + composeProject }

// Client 包装 Docker Engine API 客户端。
type Client struct {
	c *client.Client
}

// New 按环境变量（DOCKER_HOST 等）连接 Docker，并自动协商 API 版本。
func New() (*Client, error) {
	c, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Close 关闭客户端。
func (d *Client) Close() error { return d.c.Close() }

// Version 返回 Docker 引擎版本。
func (d *Client) Version(ctx context.Context) (string, error) {
	v, err := d.c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return "", err
	}
	return v.Version, nil
}

// NetworkSubnets 返回 Docker 里所有网络已占用的网段，分配项目网段时要避开。
func (d *Client) NetworkSubnets(ctx context.Context) ([]netip.Prefix, error) {
	list, err := d.c.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, n := range list.Items {
		for _, c := range n.IPAM.Config {
			if c.Subnet.IsValid() {
				out = append(out, c.Subnet)
			}
		}
	}
	return out, nil
}

// Container 是一个 compose 服务对应的容器。
type Container struct {
	ID      string
	Service string
	Image   string
	State   string
	Status  string
	Health  string
	// IPs 是容器在各个网络里的地址，键是网络名。
	IPs map[string]netip.Addr
	// Project 是 Nagi 的项目 ID（nagi.project 标签）。
	Project string
	// Member 是 Nagi 的成员名（nagi.member 标签）。
	Member string
	// Single 表示单容器成员的容器（nagi.single 标签）。
	Single  bool
	ImageID string
	// Name 是容器名（不带开头的 /）。
	Name string
}

// ManagedContainers 列出带有 labelKey 标签（Nagi 管理）的所有容器。
func (d *Client) ManagedContainers(ctx context.Context, labelKey string) ([]Container, error) {
	return d.listContainers(ctx, make(client.Filters).Add("label", labelKey), labelKey)
}

// ProjectContainers 列出某个 compose 项目的全部容器（包括已停止的）。
func (d *Client) ProjectContainers(ctx context.Context, composeProject string) ([]Container, error) {
	return d.Containers(ctx, ComposeLabel(composeProject))
}

// Containers 列出带某个标签（key=value）的全部容器（包括已停止的），例如 nagi.project=<项目 ID>。
func (d *Client) Containers(ctx context.Context, label string) ([]Container, error) {
	return d.listContainers(ctx, make(client.Filters).Add("label", label), "nagi.project")
}

func (d *Client) listContainers(ctx context.Context, filters client.Filters, projectLabel string) ([]Container, error) {
	list, err := d.c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list.Items))
	for _, c := range list.Items {
		ips := map[string]netip.Addr{}
		if c.NetworkSettings != nil {
			for name, ep := range c.NetworkSettings.Networks {
				if ep != nil && ep.IPAddress.IsValid() {
					ips[name] = ep.IPAddress
				}
			}
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Container{
			ID:      c.ID,
			Name:    name,
			Member:  c.Labels[labelMember],
			Single:  c.Labels["nagi.single"] == "1",
			Service: c.Labels[labelService],
			Image:   c.Image,
			State:   string(c.State),
			Status:  c.Status,
			Health:  health(c.Health),
			IPs:     ips,
			Project: c.Labels[projectLabel],
			ImageID: c.ImageID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Member != out[j].Member {
			return out[i].Member < out[j].Member
		}
		return out[i].Service < out[j].Service
	})
	return out, nil
}

// health 返回健康检查状态；没有健康检查时返回空字符串。
func health(h *container.HealthSummary) string {
	if h == nil || h.Status == container.NoHealthcheck {
		return ""
	}
	return string(h.Status)
}

// LogLine 是一行容器日志。
type LogLine struct {
	Member  string
	Service string
	Stream  string
	Text    string
	Time    time.Time
}

// Logs 读取一个容器的日志，每一行调用一次 onLine，直到结束或 ctx 取消。
func (d *Client) Logs(ctx context.Context, c Container, tail int, follow bool, onLine func(LogLine)) error {
	info, err := d.c.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	tailStr := "all"
	if tail > 0 {
		tailStr = strconv.Itoa(tail)
	}
	rc, err := d.c.ContainerLogs(ctx, c.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tailStr,
		Timestamps: true,
	})
	if err != nil {
		return err
	}
	defer rc.Close()

	emit := func(stream string) io.Writer {
		pr, pw := io.Pipe()
		go func() {
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				ts, text := splitTimestamp(sc.Text())
				onLine(LogLine{Service: c.Service, Stream: stream, Text: text, Time: ts})
			}
			io.Copy(io.Discard, pr)
		}()
		return pw
	}

	if info.Container.Config != nil && info.Container.Config.Tty {
		w := emit("stdout")
		_, err = io.Copy(w, rc)
		w.(*io.PipeWriter).Close()
	} else {
		stdout, stderr := emit("stdout"), emit("stderr")
		_, err = stdcopy.StdCopy(stdout, stderr, rc)
		stdout.(*io.PipeWriter).Close()
		stderr.(*io.PipeWriter).Close()
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// 开启 Timestamps 后，每行以 RFC3339Nano 时间戳加一个空格开头。
func splitTimestamp(line string) (time.Time, string) {
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		return time.Time{}, line
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, line
	}
	return t, rest
}

// WatchProjects 监听所有 Nagi 管理的容器的事件，有变化时调用 onChange。
// 出错（例如 Docker 重启）后会自动重连，直到 ctx 取消。
func (d *Client) WatchProjects(ctx context.Context, labelKey string, onChange func()) {
	for ctx.Err() == nil {
		res := d.c.Events(ctx, client.EventsListOptions{
			Filters: make(client.Filters).
				Add("type", string(events.ContainerEventType)).
				Add("label", labelKey),
		})
		msgs, errs := res.Messages, res.Err
	loop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-msgs:
				onChange()
			case <-errs:
				break loop
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// Exec 是容器里一个交互式终端会话。
type Exec struct {
	c    *client.Client
	id   string
	conn client.HijackedResponse
}

// StartExec 在容器里启动一个带 TTY 的进程。
func (d *Client) StartExec(ctx context.Context, containerID string, cmd []string, cols, rows uint) (*Exec, error) {
	created, err := d.c.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		TTY:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		ConsoleSize:  client.ConsoleSize{Height: rows, Width: cols},
		Env:          []string{"TERM=xterm-256color"},
	})
	if err != nil {
		return nil, err
	}
	att, err := d.c.ExecAttach(ctx, created.ID, client.ExecAttachOptions{
		TTY:         true,
		ConsoleSize: client.ConsoleSize{Height: rows, Width: cols},
	})
	if err != nil {
		return nil, err
	}
	return &Exec{c: d.c, id: created.ID, conn: att.HijackedResponse}, nil
}

// Read 读取终端输出。
func (e *Exec) Read(p []byte) (int, error) { return e.conn.Reader.Read(p) }

// Write 写入终端输入。
func (e *Exec) Write(p []byte) (int, error) { return e.conn.Conn.Write(p) }

// Resize 调整终端大小。
func (e *Exec) Resize(ctx context.Context, cols, rows uint) error {
	_, err := e.c.ExecResize(ctx, e.id, client.ExecResizeOptions{Width: cols, Height: rows})
	return err
}

// ExitCode 返回进程的退出码。
func (e *Exec) ExitCode(ctx context.Context) int {
	r, err := e.c.ExecInspect(ctx, e.id, client.ExecInspectOptions{})
	if err != nil {
		return -1
	}
	return r.ExitCode
}

// Close 关闭终端连接。
func (e *Exec) Close() { e.conn.Close() }

// Pause 暂停带某个标签的所有正在运行的容器，返回恢复函数。用于快照时保证数据一致。
func (d *Client) Pause(ctx context.Context, label string) (func(), error) {
	containers, err := d.Containers(ctx, label)
	if err != nil {
		return nil, err
	}
	var paused []string
	resume := func() {
		// 恢复不受调用方 ctx 取消的影响，一定要执行。
		bg := context.WithoutCancel(ctx)
		for _, id := range paused {
			_, _ = d.c.ContainerUnpause(bg, id, client.ContainerUnpauseOptions{})
		}
	}
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		if _, err := d.c.ContainerPause(ctx, c.ID, client.ContainerPauseOptions{}); err != nil {
			resume()
			return nil, err
		}
		paused = append(paused, c.ID)
	}
	return resume, nil
}

// ProjectImages 返回项目每个服务正在使用的镜像，优先用带摘要的形式（repo@sha256:...），
// 没有摘要（本地构建的镜像）时用镜像 ID。
func (d *Client) ProjectImages(ctx context.Context, composeProject string) (map[string]string, error) {
	list, err := d.c.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", ComposeLabel(composeProject)),
	})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range list.Items {
		svc := c.Labels[labelService]
		img, err := d.c.ImageInspect(ctx, c.ImageID)
		if err != nil {
			out[svc] = c.ImageID
			continue
		}
		ref := img.ID
		if len(img.RepoDigests) > 0 {
			ref = img.RepoDigests[0]
		}
		out[svc] = ref
	}
	return out, nil
}

// Stats 是一个容器的一次资源采样。
type Stats struct {
	CPUTotal    uint64 // 容器累计 CPU 时间（纳秒）
	SystemTotal uint64 // 主机累计 CPU 时间
	OnlineCPUs  uint32
	MemUsed     uint64 // 字节，不含可回收的页缓存
	MemLimit    uint64
	NetRx       uint64 // 累计字节
	NetTx       uint64
}

// ContainerStats 对容器做一次资源采样（不等待，不连续输出）。
func (d *Client) ContainerStats(ctx context.Context, id string) (Stats, error) {
	res, err := d.c.ContainerStats(ctx, id, client.ContainerStatsOptions{Stream: false})
	if err != nil {
		return Stats{}, err
	}
	defer res.Body.Close()
	var s container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		return Stats{}, err
	}
	out := Stats{
		CPUTotal:    s.CPUStats.CPUUsage.TotalUsage,
		SystemTotal: s.CPUStats.SystemUsage,
		OnlineCPUs:  s.CPUStats.OnlineCPUs,
		MemUsed:     s.MemoryStats.Usage,
		MemLimit:    s.MemoryStats.Limit,
	}
	// 页缓存可以被回收，不算作占用（cgroup v2 叫 inactive_file，v1 叫 cache）。
	if c, ok := s.MemoryStats.Stats["inactive_file"]; ok && c < out.MemUsed {
		out.MemUsed -= c
	} else if c, ok := s.MemoryStats.Stats["cache"]; ok && c < out.MemUsed {
		out.MemUsed -= c
	}
	for _, n := range s.Networks {
		out.NetRx += n.RxBytes
		out.NetTx += n.TxBytes
	}
	return out, nil
}

// RestartCount 返回容器被 Docker 自动重启的次数。
func (d *Client) RestartCount(ctx context.Context, id string) (int, error) {
	info, err := d.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return 0, err
	}
	return info.Container.RestartCount, nil
}

// Kill 立即结束带某个标签的所有正在运行的容器（SIGKILL）。顶替时来不及优雅停止才用。
func (d *Client) Kill(ctx context.Context, label string) error {
	containers, err := d.Containers(ctx, label)
	if err != nil {
		return err
	}
	var firstErr error
	for _, c := range containers {
		if c.State != "running" && c.State != "paused" && c.State != "restarting" {
			continue
		}
		if _, err := d.c.ContainerKill(ctx, c.ID, client.ContainerKillOptions{Signal: "KILL"}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Running 判断带某个标签的容器里有没有正在运行的。
func (d *Client) Running(ctx context.Context, label string) (bool, error) {
	containers, err := d.Containers(ctx, label)
	if err != nil {
		return false, err
	}
	for _, c := range containers {
		if c.State == "running" || c.State == "restarting" {
			return true, nil
		}
	}
	return false, nil
}

// SetRestartPolicy 修改容器的重启策略（不重启容器）。policy 是 no、always、unless-stopped 或 on-failure。
func (d *Client) SetRestartPolicy(ctx context.Context, id, policy string, maxRetry int) error {
	_, err := d.c.ContainerUpdate(ctx, id, client.ContainerUpdateOptions{
		RestartPolicy: &container.RestartPolicy{Name: container.RestartPolicyMode(policy), MaximumRetryCount: maxRetry},
	})
	return err
}

// ExitInfo 返回已退出容器的退出码和运行开始时间。
func (d *Client) ExitInfo(ctx context.Context, id string) (exitCode int, started time.Time, err error) {
	info, err := d.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return 0, time.Time{}, err
	}
	st := info.Container.State
	if st == nil {
		return 0, time.Time{}, nil
	}
	started, _ = time.Parse(time.RFC3339Nano, st.StartedAt)
	return st.ExitCode, started, nil
}

// StartContainer 启动一个已停止的容器。
func (d *Client) StartContainer(ctx context.Context, id string) error {
	_, err := d.c.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

// RepoDigests 返回本地镜像的仓库摘要（repo@sha256:...），本地构建的镜像没有。
func (d *Client) RepoDigests(ctx context.Context, imageID string) ([]string, error) {
	info, err := d.c.ImageInspect(ctx, imageID)
	if err != nil {
		return nil, err
	}
	return info.RepoDigests, nil
}

// RemoteDigest 向仓库查询镜像标签当前的摘要。只查询清单，不下载镜像，也不计入 Docker Hub 的拉取次数。
// encodedAuth 是私有仓库的登录信息（base64url 编码的 JSON），公开镜像为空。
func (d *Client) RemoteDigest(ctx context.Context, ref, encodedAuth string) (string, error) {
	res, err := d.c.DistributionInspect(ctx, ref, client.DistributionInspectOptions{EncodedRegistryAuth: encodedAuth})
	if err != nil {
		return "", err
	}
	return res.Descriptor.Digest.String(), nil
}

// Mirrors 返回 Docker 当前生效的镜像加速地址。
func (d *Client) Mirrors(ctx context.Context) ([]string, error) {
	info, err := d.c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, err
	}
	if info.Info.RegistryConfig == nil {
		return nil, nil
	}
	return info.Info.RegistryConfig.Mirrors, nil
}
