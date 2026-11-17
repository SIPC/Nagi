package spec

import (
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 服务字段和顶层段按白名单判断：不认识的字段（例如 use_api_socket）一律算有特权；日志驱动只放行写在容器那里的；
// label_file 和 env_file 一样只能是容器目录里的相对路径；deploy 里的设备算有特权。常见的普通写法不受影响。
func TestPrivilegeReasonsAllowList(t *testing.T) {
	svc := func(body string) string {
		return "services:\n  web:\n    image: nginx\n" + body
	}
	cases := []struct {
		name, compose string
		privileged    bool
	}{
		{"use_api_socket", svc("    use_api_socket: true\n"), true},
		{"不认识的字段", svc("    some_future_field: 1\n"), true},
		{"扩展字段", svc("    x-note: hi\n"), false},
		{"syslog 日志", svc("    logging:\n      driver: syslog\n      options:\n        syslog-address: tcp://10.0.0.5:6379\n"), true},
		{"gelf 日志", svc("    logging:\n      driver: gelf\n"), true},
		{"json-file 日志带参数", svc("    logging:\n      driver: json-file\n      options:\n        max-size: 10m\n"), false},
		{"只写参数", svc("    logging:\n      options:\n        max-size: 10m\n"), true},
		{"json-file 不认识的参数", svc("    logging:\n      driver: json-file\n      options:\n        syslog-address: tcp://x\n"), true},
		{"oom_score_adj 负数", svc("    oom_score_adj: -1000\n"), true},
		{"oom_score_adj 正数", svc("    oom_score_adj: 500\n"), false},
		{"oom_kill_disable", svc("    oom_kill_disable: true\n"), true},
		{"ulimits rtprio", svc("    ulimits:\n      rtprio: 99\n"), true},
		{"ulimits memlock", svc("    ulimits:\n      memlock: -1\n"), true},
		{"annotations", svc("    annotations:\n      a: b\n"), true},
		{"container_name", svc("    container_name: web1\n"), true},
		{"label_file 绝对路径", svc("    label_file: /etc/nagi/secret\n"), true},
		{"label_file 相对路径", svc("    label_file: ./labels\n"), false},
		{"deploy 资源", svc("    deploy:\n      resources:\n        limits:\n          cpus: '0.5'\n"), false},
		{"deploy 设备", svc("    deploy:\n      resources:\n        reservations:\n          devices:\n            - capabilities: [gpu]\n"), true},
		{"gpus", svc("    gpus: all\n"), true},
		{"blkio_config", svc("    blkio_config:\n      weight: 300\n"), true},
		{"顶层 models", svc("") + "models:\n  m:\n    model: x\n", true},
		{"顶层扩展", "x-common: &c\n  restart: always\n" + svc("    <<: *c\n"), false},
		{"常见写法", svc("    restart: unless-stopped\n    environment:\n      A: b\n    ports:\n      - \"8080:80\"\n    depends_on:\n      - db\n    healthcheck:\n      test: [\"CMD\", \"true\"]\n    labels:\n      a: b\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n    ulimits:\n      nofile: 65535\n    sysctls:\n      net.core.somaxconn: 1024\n    shm_size: 256m\n    stop_grace_period: 30s\n    tmpfs:\n      - /run\n") + "  db:\n    image: postgres\n", false},
	}
	for _, c := range cases {
		reasons, _ := PrivilegeReasons(&nagiv1.ContainerConfig{Compose: c.compose}, "web")
		if (len(reasons) > 0) != c.privileged {
			t.Errorf("%s：reasons = %v，应为 privileged=%v", c.name, reasons, c.privileged)
		}
	}
}
