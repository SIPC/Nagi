package agent

import (
	"strings"
	"testing"
)

func TestExplain(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"docker compose 失败：Error response from daemon: pull access denied for x/y, repository does not exist or may require 'docker login'", "镜像不存在"},
		{"docker compose 失败：manifest for nginx:9.9 not found: manifest unknown: manifest unknown", "没有这个标签"},
		{"拉取镜像: Get \"https://registry-1.docker.io/v2/\": net/http: TLS handshake timeout", "连不上镜像仓库"},
		{"write /srv/a/config/project.db: no space left on device", "磁盘空间不足"},
		// 不是拉取镜像的错误，不套用 Docker 的提示。
		{"连接面板: connection refused", ""},
		{"disk I/O error (4874)", ""}, // 看不出是磁盘满（测试里没有满的磁盘），原样返回
	}
	for _, c := range cases {
		got := explain(c.msg)
		if c.want == "" {
			if got != c.msg {
				t.Errorf("explain(%q) = %q，应该原样返回", c.msg, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) || !strings.Contains(got, c.msg) {
			t.Errorf("explain(%q) = %q，应该包含 %q 和原始错误", c.msg, got, c.want)
		}
	}
}
