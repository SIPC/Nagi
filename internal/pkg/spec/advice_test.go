package spec

import (
	"reflect"
	"testing"
)

func TestAdviseComposeEnvironment(t *testing.T) {
	a, err := AdviseCompose(`services:
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: s3cret
      POSTGRES_USER: ${DB_USER}
      PORT: 5432
      EMPTY: ""
      FROM_ENV:
      ESCAPED: "a$$b"
  web:
    image: shop
    environment:
      - DATABASE_URL=postgres://app:${DB_PASSWORD}@db:5432/app
      - NODE_ENV=production
      - TOKEN
      - SHORT=$HOME
  worker:
    image: shop
    environment:
      A: ${A}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"db": {"ESCAPED", "PORT", "POSTGRES_PASSWORD"}, "web": {"NODE_ENV"}}
	if !reflect.DeepEqual(a.LiteralEnv, want) {
		t.Errorf("写死的变量 = %v，应为 %v", a.LiteralEnv, want)
	}
	if !reflect.DeepEqual(a.Services, []string{"db", "web", "worker"}) {
		t.Errorf("服务 = %v", a.Services)
	}
	if len(a.EnvFiles) != 0 || len(a.HostPaths) != 0 {
		t.Errorf("不该有 env_file 和宿主机路径：%v %v", a.EnvFiles, a.HostPaths)
	}
}

func TestAdviseComposeEnvFile(t *testing.T) {
	a, err := AdviseCompose(`services:
  web:
    image: shop
    env_file: .env
  api:
    image: shop
    env_file:
      - path: ./api.env
        required: false
      - common.env
  db:
    image: postgres:17
`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"web": {".env"}, "api": {"./api.env", "common.env"}}
	if !reflect.DeepEqual(a.EnvFiles, want) {
		t.Errorf("env_file = %v，应为 %v", a.EnvFiles, want)
	}
}

func TestAdviseComposeHostPaths(t *testing.T) {
	a, err := AdviseCompose(`services:
  db:
    image: postgres:17
    volumes:
      - /opt/pg:/var/lib/postgresql/data
      - ./init:/docker-entrypoint-initdb.d:ro
      - /etc/localtime:/etc/localtime:ro
  web:
    image: shop
    volumes:
      - ~/uploads:/app/uploads
      - ../shared:/app/shared
      - uploads:/app/data
      - ${MEDIA_DIR}:/media
      - /var/run/docker.sock:/var/run/docker.sock
      - type: bind
        source: /data/web
        target: /app/cache
      - type: bind
        source: ./logs
        target: /app/logs
      - /cache
volumes:
  uploads:
  pgdata:
    driver: local
    driver_opts:
      type: none
      o: bind
      device: /root/pgdata
  ram:
    driver_opts:
      type: tmpfs
      device: tmpfs
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []HostPath{
		{Volume: "pgdata", Source: "/root/pgdata"},
		{Service: "db", Source: "/opt/pg"},
		{Service: "web", Source: "../shared"},
		{Service: "web", Source: "/data/web"},
		{Service: "web", Source: "~/uploads"},
	}
	if !reflect.DeepEqual(a.HostPaths, want) {
		t.Errorf("宿主机路径 = %v，应为 %v", a.HostPaths, want)
	}
}

func TestAdviseComposeClean(t *testing.T) {
	a, err := AdviseCompose(`services:
  app:
    image: umami
    environment:
      DATABASE_URL: postgresql://umami:${DB_PASSWORD}@db:5432/umami
    volumes:
      - ./uploads:/app/uploads
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes:
      - db-data:/var/lib/postgresql/data
volumes:
  db-data:
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.LiteralEnv) != 0 || len(a.EnvFiles) != 0 || len(a.HostPaths) != 0 || len(a.Services) != 2 {
		t.Errorf("没有不建议的写法，却得到 %+v", a)
	}
	if _, err := AdviseCompose("services: [\n"); err == nil {
		t.Error("解析不了的 compose 应该返回错误")
	}
}

func TestLooksSecret(t *testing.T) {
	for name, want := range map[string]bool{"POSTGRES_PASSWORD": true, "API_TOKEN": true, "APP_SECRET": true, "aws_access_key_id": true, "NODE_ENV": false, "PORT": false} {
		if LooksSecret(name) != want {
			t.Errorf("LooksSecret(%q) 应为 %v", name, want)
		}
	}
}

func TestAdviseComposeFields(t *testing.T) {
	a, err := AdviseCompose(`services:
  web:
    build: .
    container_name: web
    network_mode: host
    ports:
      - "127.0.0.1:8080:80"
      - target: 443
        published: 8443
        host_ip: 0.0.0.0
      - "${PORT}:3000"
    extra_hosts:
      - host.docker.internal:host-gateway
    environment:
      DB: postgres://u:${P}@10.0.0.5/app
  db:
    image: postgres:16
    ports: ["5432:5432", "9000"]
    volumes: ["pg:/var/lib/postgresql/data"]
  files:
    image: nginx
    volumes: ["./html:/usr/share/nginx/html"]
  cache:
    image: redis
    volumes: ["/data"]
volumes:
  pg:
  old:
    external: true
networks:
  shared:
    external:
      name: shared
  own:
`)
	if err != nil {
		t.Fatal(err)
	}
	wantPorts := []PortUse{{Service: "db", Target: 5432, Published: "5432"}, {Service: "db", Target: 9000},
		{Service: "web", Target: 80, Published: "8080", HostIP: "127.0.0.1"}, {Service: "web", Target: 443, Published: "8443", HostIP: "0.0.0.0"}}
	if !reflect.DeepEqual(a.Ports, wantPorts) {
		t.Errorf("端口 = %+v，应为 %+v", a.Ports, wantPorts)
	}
	checks := map[string][2]any{
		"DataServices":     {a.DataServices, []string{"db", "files"}},
		"Build":            {a.Build, []string{"web"}},
		"ContainerName":    {a.ContainerName, []string{"web"}},
		"HostNetwork":      {a.HostNetwork, []string{"web"}},
		"ExternalVolumes":  {a.ExternalVolumes, []string{"old"}},
		"ExternalNetworks": {a.ExternalNetworks, []string{"shared"}},
		"ExtraHosts":       {a.ExtraHosts, map[string][]string{"web": {"host.docker.internal:host-gateway"}}},
		"EnvValues":        {a.EnvValues, map[string]map[string]string{"web": {"DB": "postgres://u:${P}@10.0.0.5/app"}}},
		"Images":           {a.Images, map[string]string{"db": "postgres:16", "files": "nginx", "cache": "redis"}},
	}
	for name, c := range checks {
		if !reflect.DeepEqual(c[0], c[1]) {
			t.Errorf("%s = %v，应为 %v", name, c[0], c[1])
		}
	}
}
