package spec

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 多个 YAML 文档、自定义标签：compose 会读到后面的文档、按标签重置或覆盖，第一个文档的普通 map 里看不出来，
// 一律算有特权（只有管理员能保存）。
func TestPrivilegeReasonsYAMLShape(t *testing.T) {
	cases := []struct {
		name, compose string
		privileged    bool
	}{
		{"多文档藏特权", "services:\n  web:\n    image: nginx\n---\nservices:\n  web:\n    privileged: true\n    volumes:\n      - /:/host\n", true},
		{"前面一个空文档", "---\n---\nservices:\n  web:\n    image: nginx\n    privileged: true\n", true},
		{"前面一个 null 文档", "~\n---\nservices:\n  web:\n    image: nginx\n", true},
		{"开头的分隔符", "---\nservices:\n  web:\n    image: nginx\n", false},
		{"末尾空文档", "services:\n  web:\n    image: nginx\n---\n", false},
		{"override 标签", "services:\n  web:\n    image: nginx\n    volumes: !override\n      - ./a:/a\n", true},
		{"reset 标签", "services:\n  web:\n    image: nginx\n    command: !reset []\n", true},
		{"标准标签", "services:\n  web:\n    image: !!str nginx\n", false},
		{"锚点和合并", "x-base: &b\n  image: nginx\nservices:\n  web:\n    <<: *b\n", false},
		{"锚点合并进特权", "x-base: &b\n  privileged: true\nservices:\n  web:\n    image: nginx\n    <<: *b\n", true},
	}
	for _, c := range cases {
		cfg := &nagiv1.ContainerConfig{Compose: c.compose}
		reasons, _ := PrivilegeReasons(cfg, "web")
		if (len(reasons) > 0) != c.privileged {
			t.Errorf("%s：reasons = %v，应为 privileged=%v", c.name, reasons, c.privileged)
		}
	}
}
