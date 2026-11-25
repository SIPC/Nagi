package metrics

import (
	"slices"
	"strings"
	"testing"
)

const sampleMountInfo = `21 26 0:20 / /proc rw,nosuid - proc proc rw
25 26 0:23 / /run rw,nosuid shared:5 - tmpfs tmpfs rw
26 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
27 26 8:1 / /boot/efi rw,relatime shared:2 - vfat /dev/sda1 rw
28 26 8:17 / /data\0401 rw,relatime shared:3 - xfs /dev/sdb1 rw
29 26 8:33 / /mnt/backup rw,relatime - ext4 /dev/sdc1 rw
30 26 8:33 /sub /srv/backup-bind rw,relatime - ext4 /dev/sdc1 rw
31 26 8:49 / /mnt/media rw,relatime - btrfs /dev/sdd rw
32 26 8:2 /var/lib/docker /var/lib/docker rw,relatime - ext4 /dev/sda2 rw
33 26 7:0 / /snap/core/1 ro,nodev - squashfs /dev/loop0 ro
34 26 0:50 / /mnt/nas rw - nfs4 10.0.0.1:/share rw
35 26 0:60 / /tank rw - zfs tank rw
36 26 0:61 / /tank/app rw - zfs tank/app rw
37 26 0:70 / /var/lib/docker/overlay2/x/merged rw - overlay overlay rw
`

func TestSelectDisks(t *testing.T) {
	entries := parseMountInfo(strings.NewReader(sampleMountInfo))
	got := selectDisks(entries, nil)
	want := []string{"/", "/data 1", "/mnt/backup", "/mnt/media", "/tank"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSelectDisksIncludesPathsInUse(t *testing.T) {
	entries := parseMountInfo(strings.NewReader(sampleMountInfo))
	// 项目目录在网络存储上：要算上。节点目录在根盘上：不重复。
	got := selectDisks(entries, []string{"/var/lib/nagi", "/mnt/nas/projects", "/mnt/nasty"})
	want := []string{"/", "/data 1", "/mnt/backup", "/mnt/media", "/mnt/nas", "/tank"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSelectDisksInContainer(t *testing.T) {
	// 容器里根目录是 overlay，块设备只有绑定挂载的文件：退回节点目录所在的文件系统。
	entries := parseMountInfo(strings.NewReader(`100 90 0:80 / / rw - overlay overlay rw
101 100 8:48 /x/resolv.conf /etc/resolv.conf rw - ext4 /dev/sdd rw
`))
	if got := selectDisks(entries, []string{"/var/lib/nagi"}); !slices.Equal(got, []string{"/"}) {
		t.Fatalf("got %q", got)
	}
}

func TestDiskRatesCountWholePhysicalDisksOnly(t *testing.T) {
	physical := func(name string) bool { return name == "sda" || name == "nvme0n1" }
	// 主 次 名称 读次数 合并 读扇区 读耗时 写次数 合并 写扇区 …
	before := parseDiskstats(strings.NewReader(`   8       0 sda 100 0 2000 0 50 0 4000 0 0 0 0
   8       1 sda1 90 0 1800 0 40 0 3000 0 0 0 0
 259       0 nvme0n1 10 0 80 0 5 0 16 0 0 0 0
 253       0 dm-0 999 0 99999 0 999 0 99999 0 0 0 0
   7       0 loop0 5 0 10 0 0 0 0 0 0 0 0
`), physical)
	if len(before) != 2 || before["8.0"].readBytes != 2000*512 || before["259.0"].writes != 5 {
		t.Fatalf("parsed %+v", before)
	}
	after := parseDiskstats(strings.NewReader(`   8       0 sda 130 0 2600 0 70 0 9000 0 0 0 0
   8       1 sda1 120 0 2400 0 60 0 8000 0 0 0 0
 259       0 nvme0n1 3 0 8 0 1 0 2 0 0 0 0
   8      16 sdb 400 0 900000 0 400 0 900000 0 0 0 0
 253       0 dm-0 2000 0 199999 0 2000 0 199999 0 0 0 0
`), func(name string) bool { return physical(name) || name == "sdb" })
	got := diskRates(before, after)
	// 只算 sda：分区和 dm-0 不重复算，nvme0n1 计数变小（重新接上）、sdb 刚出现，这一轮都不算。
	if got != [4]float64{600 * 512, 5000 * 512, 30, 20} {
		t.Fatalf("got %v", got)
	}
}

func TestContainerDiskIOPrefersPhysicalDisk(t *testing.T) {
	physical := map[string]diskIO{"8.0": {}}
	prev := map[string]float64{"device@8.0:io_bytes_read_total": 100, "device@8.0:io_bytes_write_total": 1000, "device@8.0:io_ops_write_total": 1,
		"device@253.0:io_bytes_read_total": 100, "device@253.0:io_bytes_write_total": 1000, "device@253.0:io_ops_write_total": 1}
	cur := map[string]float64{"device@8.0:io_bytes_read_total": 400, "device@8.0:io_bytes_write_total": 5000, "device@8.0:io_ops_write_total": 9,
		"device@253.0:io_bytes_read_total": 400, "device@253.0:io_bytes_write_total": 5000, "device@253.0:io_ops_write_total": 9,
		"device@8.0:io_service_nanoseconds_write_total": 1e9, "memory_usage_bytes": 1 << 30}
	// 同一份读写既记在 dm-0 上又记在 sda 上：只算 sda 的。读次数没报（cgroup v2 上 Docker 不报次数）：缺失，不是 0。
	if got, ok := containerDiskIO(prev, cur, physical); ok != [4]bool{true, true, false, true} || got != [4]float64{300, 4000, 0, 8} {
		t.Fatalf("got %v %v", got, ok)
	}
	// 读不到 /proc/diskstats 时算全部设备。
	if got, _ := containerDiskIO(prev, cur, nil); got != [4]float64{600, 8000, 0, 16} {
		t.Fatalf("without diskstats got %v", got)
	}
	// 容器重建（计数变小）这一轮不算，不出现负数。
	if got, ok := containerDiskIO(cur, prev, physical); !ok[0] || got != [4]float64{} {
		t.Fatalf("reset got %v %v", got, ok)
	}
	// Docker 没报读写统计：没有数据，不是 0。
	if _, ok := containerDiskIO(nil, map[string]float64{"pids": 3, "device@8.0:io_wait_nanoseconds_read_total": 1}, physical); ok != [4]bool{} {
		t.Fatal("missing blkio stats must not be recorded as zero")
	}
}
