package release

import "testing"

func TestIsVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.1.0": true, "v1.20.3": true, "v0.2.0-rc.1": true,
		"dev": false, "": false, "0.1.0": false, "v1.0": false, "v0.1.0/../x": false,
	} {
		if got := IsVersion(v); got != want {
			t.Errorf("IsVersion(%q) = %v，应为 %v", v, got, want)
		}
	}
}

func TestDownloadBase(t *testing.T) {
	if got := DownloadBase("v0.1.0"); got != "https://github.com/SIPC/Nagi/releases/download/v0.1.0" {
		t.Errorf("正式版本：%s", got)
	}
	if got := DownloadBase("dev"); got != "https://github.com/SIPC/Nagi/releases/latest/download" {
		t.Errorf("开发版本：%s", got)
	}
}

func TestChecksum(t *testing.T) {
	sums := "aaa  nagi-linux-amd64\nBBB *nagi-linux-arm64\nccc  install.sh\n"
	if got := Checksum(sums, "nagi-linux-amd64"); got != "aaa" {
		t.Errorf("amd64：%q", got)
	}
	if got := Checksum(sums, "nagi-linux-arm64"); got != "bbb" {
		t.Errorf("arm64（二进制模式、大写）：%q", got)
	}
	if got := Checksum(sums, "nagi-linux-riscv64"); got != "" {
		t.Errorf("没有的文件：%q", got)
	}
}
