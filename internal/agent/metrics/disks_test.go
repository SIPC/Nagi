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
