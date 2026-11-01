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
