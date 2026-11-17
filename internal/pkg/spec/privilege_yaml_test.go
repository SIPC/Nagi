package spec

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 普通的 YAML 解析和 compose 理解不一样的写法（多个文档、显式标签、%TAG 指令、合并键），一律算有特权（只有管理员能保存）；
// 另外按 compose 自己的解析结果再检查一遍。
func TestPrivilegeReasonsYAMLShape(t *testing.T) {
	cases := []struct {
		name, compose string
		privileged    bool
	}{
		{"多文档藏特权", "services:\n  web:\n    image: nginx\n---\nservices:\n  web:\n    privileged: true\n    volumes:\n      - /:/host\n", true},
		{"前面一个空文档", "---\n---\nservices:\n  web:\n    image: nginx\n    privileged: true\n", true},
		{"前面一个 null 文档", "~\n---\nservices:\n  web:\n    image: nginx\n", true},
		{"开头的分隔符", "---\nservices:\n  web:\n    image: nginx\n", false},
		// compose 自己也解析不了末尾的空文档（部署会失败），算有特权不影响使用。
		{"末尾空文档", "services:\n  web:\n    image: nginx\n---\n", true},
		{"override 标签", "services:\n  web:\n    image: nginx\n    volumes: !override\n      - ./a:/a\n", true},
		{"reset 标签", "services:\n  web:\n    image: nginx\n    command: !reset []\n", true},
		{"标准标签", "services:\n  web:\n    image: !!str nginx\n", true},
		// H1：带 !!merge 的普通键，compose 当作合并键展开；%TAG 能把自定义前缀展开成标准标签。
		{"merge 标签的普通键", "x-p: &p\n  privileged: true\n  volumes:\n    - /:/host\nservices:\n  web:\n    image: nginx\n    !!merge foo: *p\n", true},
		{"TAG 指令", "%TAG !e! tag:yaml.org,2002:\n---\nx-p: &p\n  privileged: true\nservices:\n  web:\n    image: nginx\n    !e!merge foo: *p\n", true},
		{"merge 标签的引号键", "x-p: &p\n  privileged: true\nservices:\n  web:\n    image: nginx\n    !!merge \"<<\": *p\n", true},
		{"锚点和合并", "x-base: &b\n  image: nginx\nservices:\n  web:\n    <<: *b\n", false},
		{"锚点合并进特权", "x-base: &b\n  privileged: true\nservices:\n  web:\n    image: nginx\n    <<: *b\n", true},
		{"普通", "services:\n  web:\n    image: nginx\n    volumes:\n      - ./data:/data\n", false},
	}
	for _, c := range cases {
		cfg := &nagiv1.ContainerConfig{Compose: c.compose}
		reasons, _ := PrivilegeReasons(cfg, "web")
		if (len(reasons) > 0) != c.privileged {
			t.Errorf("%s：reasons = %v，应为 privileged=%v", c.name, reasons, c.privileged)
		}
	}
}

// compose 的解析结果里看到的特权，原文的普通解析看不到时也要报出来（不依赖上面的写法检查）。
func TestComposeModelSeesMerge(t *testing.T) {
	text := "x-p: &p\n  privileged: true\nservices:\n  web:\n    image: nginx\n    !!merge foo: *p\n"
	model, err := composeModel(text)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	checkDoc(model, func(_ bool, format string, args ...any) { reasons = append(reasons, format) })
	if len(reasons) == 0 {
		t.Fatalf("compose 的解析结果里应该看到 privileged：%v", model)
	}
}
