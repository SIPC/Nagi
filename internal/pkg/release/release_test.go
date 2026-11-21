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

// 正式版渠道永远不装预发布版；认不出的渠道（例如空值、旧数据）按正式版渠道处理。
func TestAllowed(t *testing.T) {
	cases := []struct {
		channel, v string
		want       bool
	}{
		{ChannelStable, "v0.37.1", true},
		{ChannelStable, "v0.37.1-pre.1", false},
		{"", "v0.37.1-pre.1", false},
		{"beta", "v0.37.1-pre.1", false},
		{ChannelPre, "v0.37.1-pre.1", true},
		{ChannelPre, "v0.37.1", true},
		{ChannelPre, "dev", false},
		{ChannelStable, "v1.0.0+build.1", true},
	}
	for _, c := range cases {
		if got := Allowed(c.channel, c.v); got != c.want {
			t.Errorf("Allowed(%q, %q) = %v，应为 %v", c.channel, c.v, got, c.want)
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
