package spec

import (
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

func TestPrivilegeReasons(t *testing.T) {
	svc := func(body string) string {
		return "services:\n  web:\n    image: nginx\n" + body
	}
	cases := []struct {
		name    string
		compose string
		want    string // 空表示没有特权
		dynamic bool
	}{
		{"普通", svc(""), "", false},
		{"相对路径和命名卷", svc("    volumes:\n      - ./data:/d\n      - cache:/c\n      - /anon\n") + "volumes:\n  cache: {}\n", "", false},
		{"长写法 bind 相对路径", svc("    volumes:\n      - type: bind\n        source: ./x\n        target: /x\n"), "", false},
		{"tmpfs", svc("    volumes:\n      - type: tmpfs\n        target: /x\n"), "", false},
		{"privileged false", svc("    privileged: false\n"), "", false},
		{"network_mode 另一个服务", svc("    network_mode: service:db\n") + "  db:\n    image: postgres\n", "", false},
		{"network_mode none", svc("    network_mode: none\n"), "", false},
		{"ipc private", svc("    ipc: private\n"), "", false},
		{"相对路径的 env_file", svc("    env_file: ./a.env\n"), "", false},
		{"并列的挂载", svc("    volumes:\n      - ./a:/a\n      - ./ab:/ab\n") + "  db:\n    image: postgres\n    volumes:\n      - ./a:/a\n", "", false},

		{"privileged", svc("    privileged: true\n"), "web: privileged", false},
		{"privileged 变量", svc("    privileged: ${P}\n"), "web: privileged", true},
		{"cap_add", svc("    cap_add: [SYS_ADMIN]\n"), "web: cap_add", false},
		{"devices", svc("    devices:\n      - /dev/sda:/dev/sda\n"), "web: devices", false},
		{"build", svc("    build: .\n"), "web: build", false},
		{"pid host", svc("    pid: host\n"), "web: pid", false},
		{"network host", svc("    network_mode: host\n"), "web: network_mode", false},
		{"network 变量", svc("    network_mode: ${N}\n"), "web: network_mode", true},
		{"共用别的容器的网络", svc("    network_mode: container:other\n"), "web: network_mode", false},
		{"共用别的容器的进程", svc("    pid: container:other\n"), "web: pid", false},
		{"共用别的容器的 ipc", svc("    ipc: container:other\n"), "web: ipc", false},
		{"不在这个 compose 里的服务", svc("    network_mode: service:db\n"), "web: network_mode", false},
		{"接进别的网络", svc("    network_mode: nagi-other\n"), "web: network_mode", false},
		{"指定名字的网络", svc("") + "networks:\n  n:\n    name: nagi-other\n", "networks: n", false},
		{"指定名字的卷", svc("    volumes:\n      - x:/x\n") + "volumes:\n  x:\n    name: other_data\n", "volumes: x", false},
		{"挂载根目录", svc("    volumes:\n      - /:/host\n"), "web: volumes", false},
		{"docker.sock", svc("    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"), "web: volumes", false},
		{"家目录", svc("    volumes:\n      - ~/.ssh:/s\n"), "web: volumes", false},
		{"跳出容器目录", svc("    volumes:\n      - ../../etc:/e\n"), "web: volumes", false},
		{"藏在 ./ 后面跳出", svc("    volumes:\n      - ./a/../../etc:/e\n"), "web: volumes", false},
		{"挂载路径是变量", svc("    volumes:\n      - ${DIR}:/d\n"), "web: volumes", true},
		{"长写法 bind 绝对路径", svc("    volumes:\n      - type: bind\n        source: /etc\n        target: /e\n"), "web: volumes", false},
		{"读主机上的 env_file", svc("    env_file: /etc/shadow\n"), "web: env_file", false},
		{"长写法 env_file", svc("    env_file:\n      - path: ../x.env\n"), "web: env_file", false},
		{"读主机上的 secret", svc("") + "secrets:\n  s:\n    file: /etc/shadow\n", "secrets: s", false},
		{"bind 的命名卷", svc("    volumes:\n      - x:/x\n") + "volumes:\n  x:\n    driver_opts:\n      device: /etc\n      o: bind\n      type: none\n", "volumes: x", false},
		{"外部网络", svc("") + "networks:\n  n:\n    external: true\n", "networks: n", false},
		{"include", "include:\n  - /etc/x.yml\n" + svc(""), "include", false},
		{"合并写法藏着特权", "x-a: &a\n  privileged: true\nservices:\n  web:\n    <<: *a\n    image: nginx\n", "web: privileged", false},
		{"嵌套的挂载", svc("    volumes:\n      - .:/app\n      - ./uploads:/app/uploads\n"), "web: volumes（. 包含 uploads）", false},
		{"跨服务嵌套的挂载", svc("    volumes:\n      - ./a:/a\n") + "  db:\n    image: postgres\n    volumes:\n      - type: bind\n        source: ./a/b\n        target: /b\n", "web: volumes（a 包含 a/b）", false},
		{"挂载包含 secret 的文件", svc("    volumes:\n      - ./conf:/c\n") + "secrets:\n  s:\n    file: ./conf/s\n", "web: volumes（conf 包含 conf/s）", false},
		{"读不懂", "services: [", "compose", false},
		{"服务不是表", "services:\n  web: 1\n", "web", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, dyn := PrivilegeReasons(&nagiv1.ContainerConfig{Compose: c.compose}, "web")
			if s := strings.Join(got, ","); s != c.want {
				t.Fatalf("结果 %q，应该是 %q", s, c.want)
			}
			if dyn != c.dynamic {
				t.Fatalf("dynamic = %v，应该是 %v", dyn, c.dynamic)
			}
		})
	}
}

func TestPrivilegeReasonsForm(t *testing.T) {
	cfg := &nagiv1.ContainerConfig{Form: &nagiv1.ImageForm{Image: "nginx", Volumes: []string{"/var/lib/x"}}}
	if got, _ := PrivilegeReasons(cfg, "web"); len(got) != 0 {
		t.Fatalf("镜像表单不应该有特权：%v", got)
	}
}
