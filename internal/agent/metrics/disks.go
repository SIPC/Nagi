package metrics

import (
	"bufio"
	"cmp"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Disk 是节点上一块磁盘的容量和用量（字节）。
type Disk struct {
	Mount string
	Total uint64
	Used  uint64
}

// diskIO 是一块磁盘自开机以来累计读写的字节数和次数（/proc/diskstats）。
type diskIO struct{ readBytes, writeBytes, reads, writes uint64 }

// parseDiskstats 解析 /proc/diskstats，只留整块的物理磁盘（physical 按设备名判断），键是「主.次设备号」，
// 和 Docker 按设备报的容器读写（device@8.0）同一种写法。分区、device-mapper、md、loop 上的读写最终都落在物理盘上，
// 再算一遍就重复了。格式见内核的 Documentation/admin-guide/iostats.rst：主 次 名称 读次数 合并 读扇区 读耗时 写次数 合并 写扇区 …，
// 扇区固定按 512 字节算。
func parseDiskstats(r io.Reader, physical func(name string) bool) map[string]diskIO {
	out := map[string]diskIO{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || !physical(f[2]) {
			continue
		}
		var n [4]uint64
		ok := true
		for i, col := range []int{3, 5, 7, 9} {
			var err error
			if n[i], err = strconv.ParseUint(f[col], 10, 64); err != nil {
				ok = false
			}
		}
		if ok {
			out[f[0]+"."+f[1]] = diskIO{reads: n[0], readBytes: n[1] * 512, writes: n[2], writeBytes: n[3] * 512}
		}
	}
	return out
}

// diskRates 是两次采样之间各磁盘读写的增量之和（顺序同 diskIOMetrics）。前一次没有的盘（刚接上）和计数变小的盘（重新接上）这一轮不算，
// 不会把开机以来的累计当成这 10 秒的读写。
func diskRates(prev, cur map[string]diskIO) (out [4]float64) {
	for dev, x := range cur {
		p, ok := prev[dev]
		if !ok || x.readBytes < p.readBytes || x.writeBytes < p.writeBytes || x.reads < p.reads || x.writes < p.writes {
			continue
		}
		out[0] += float64(x.readBytes - p.readBytes)
		out[1] += float64(x.writeBytes - p.writeBytes)
		out[2] += float64(x.reads - p.reads)
		out[3] += float64(x.writes - p.writes)
	}
	return out
}

// containerDiskIO 是容器两次采样之间读写的字节数和次数（顺序同 diskIOMetrics），来自 Docker 按设备报的累计值
// （cgroup v2 的 io.stat，v1 的 blkio.io_service_bytes_recursive、io_serviced_recursive），键形如 device@8.0:io_bytes_read_total。
// 同一次读写会同时记在 device-mapper 等叠加设备和下面的物理盘上，所以容器在物理盘（physical 的键）上有记录时只算物理盘；
// 一块都对不上（例如读不到 /proc/diskstats）时算全部设备。Docker 没报的项（拿不到 cgroup 的读写统计；cgroup v2 上 Docker\n// 只报字节、不报次数）ok 为 false，不记成 0。
func containerDiskIO(prev, cur map[string]float64, physical map[string]diskIO) (out [4]float64, ok [4]bool) {
	var all, phys [4]float64
	onPhysical := false
	for key, x := range cur {
		rest, found := strings.CutPrefix(key, "device@")
		dev, metric, _ := strings.Cut(rest, ":")
		i := slices.Index(dockerIOKeys[:], metric)
		if !found || i < 0 {
			continue
		}
		ok[i] = true
		_, isPhysical := physical[dev]
		onPhysical = onPhysical || isPhysical
		before, seen := prev[key]
		if !seen || x < before {
			continue
		}
		all[i] += x - before
		if isPhysical {
			phys[i] += x - before
		}
	}
	if onPhysical {
		return phys, ok
	}
	return all, ok
}

// diskIOMetrics 是节点（node:）和容器（c:…:）的磁盘读写序列名：读、写（字节/秒），读、写次数（次/秒）。
var diskIOMetrics = [4]string{"disk_read", "disk_write", "disk_read_ops", "disk_write_ops"}

// dockerIOKeys 是 docker.Stats.Details 里和 diskIOMetrics 对应的累计值。
var dockerIOKeys = [4]string{"io_bytes_read_total", "io_bytes_write_total", "io_ops_read_total", "io_ops_write_total"}

// mountEntry 是 /proc/self/mountinfo 里的一行。
type mountEntry struct {
	mount  string
	dev    string // 主次设备号，例如 8:1。
	fstype string
	source string
}

// 这些目录下的挂载不是用户的数据盘：启动分区、系统目录、容器运行时和 snap 的绑定挂载。
var ignoredMounts = []string{"/boot", "/dev", "/etc", "/proc", "/run", "/snap", "/sys", "/var/lib/containerd", "/var/lib/docker", "/var/lib/kubelet"}

// parseMountInfo 解析 /proc/self/mountinfo，格式见 proc(5)：
// 36 35 98:0 /root /mnt rw,noatime master:1 - ext3 /dev/sda1 rw。"-" 之前的可选字段数量不定。
func parseMountInfo(r io.Reader) []mountEntry {
	var out []mountEntry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := slices.Index(fields, "-")
		if sep < 5 || len(fields) < sep+3 {
			continue
		}
		out = append(out, mountEntry{mount: unescapeMount(fields[4]), dev: fields[2], fstype: fields[sep+1], source: fields[sep+2]})
	}
	return out
}

// selectDisks 从挂载列表里选出要统计的磁盘，返回挂载点：只算本地块设备和 ZFS，同一块盘只留最短的挂载点。
// paths（节点目录、项目目录）所在的盘即使不符合条件（例如网络存储）也算上，它们是节点实际在用的盘。
func selectDisks(entries []mountEntry, paths []string) []string {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b mountEntry) int {
		return cmp.Or(cmp.Compare(len(a.mount), len(b.mount)), cmp.Compare(a.mount, b.mount))
	})
	var out []string
	seen := map[string]bool{}
	add := func(e mountEntry) {
		if key := e.diskKey(); !seen[key] {
			seen[key] = true
			out = append(out, e.mount)
		}
	}
	for _, e := range sorted {
		if e.isLocalDisk() {
			add(e)
		}
	}
	for _, path := range paths {
		// 排过序，最后一个前缀匹配的就是最长的挂载点。
		var best *mountEntry
		for i, e := range sorted {
			if path == e.mount || e.mount == "/" || strings.HasPrefix(path, e.mount+"/") {
				best = &sorted[i]
			}
		}
		if best != nil {
			add(*best)
		}
	}
	slices.Sort(out)
	return out
}

// diskKey 标识一块盘。ZFS 的数据集共用存储池的空间，按存储池算。
func (e mountEntry) diskKey() string {
	if e.fstype == "zfs" {
		pool, _, _ := strings.Cut(e.source, "/")
		return "zfs:" + pool
	}
	return e.dev
}

func (e mountEntry) isLocalDisk() bool {
	for _, dir := range ignoredMounts {
		if e.mount == dir || strings.HasPrefix(e.mount, dir+"/") {
			return false
		}
	}
	switch e.fstype {
	case "zfs":
		return true
	case "squashfs", "iso9660", "udf":
		return false
	}
	return strings.HasPrefix(e.source, "/dev/") && !strings.HasPrefix(e.source, "/dev/loop") && !strings.HasPrefix(e.source, "/dev/ram")
}

// unescapeMount 还原 mountinfo 里转义的字符（例如空格写成 \040）。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
