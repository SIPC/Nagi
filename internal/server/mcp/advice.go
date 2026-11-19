package mcp

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 保存后给 agent 的提醒（见 docs/mcp.md#给-agent-的说明）：配置照常保存，结果里多一项 warnings，说明问题和怎么改。
// 规则和 nagi://docs/deploy 一致：单个服务用镜像表单，值放进 Nagi 的变量，数据留在容器目录里，内部服务不开放端口。
// 管理员可以合法地这样写（用户给的 compose、有意挂载的宿主机目录），所以只提醒、不拒绝，措辞留余地。

// adviceOpts 是检查时要知道的上下文。
type adviceOpts struct {
	// imported：导入现有目录时 compose 是用户原有的，不提醒单个服务；它的 env_file 由导入换成面板的变量，也不提醒；
	// 相对路径指向原来的目录，宿主机路径的建议跟着改。
	imported bool
	// deployed：容器部署过，ports 已经导入成映射，再改不生效。oldPorts 是改之前的端口，用来判断这次改没改端口。
	deployed bool
	oldPorts []string
}

// internalPorts 是数据库、缓存、搜索、消息队列这类只给项目里的容器用的服务的常见端口。
var internalPorts = map[uint32]string{5432: "PostgreSQL", 3306: "MySQL/MariaDB", 1433: "SQL Server", 6379: "Redis", 27017: "MongoDB",
	9200: "Elasticsearch", 9300: "Elasticsearch", 11211: "Memcached", 5672: "RabbitMQ", 9042: "Cassandra"}

// portKey 是比较端口有没有改用的：服务/容器端口/对外端口。
func portKey(p spec.PortUse) string { return fmt.Sprintf("%s/%d/%s", p.Service, p.Target, p.Published) }

// portWarnings 检查端口：内部服务不开放端口、host_ip 不起作用、80 和 443 留给域名、部署过的容器改端口不生效。
func portWarnings(ports []spec.PortUse, o adviceOpts) []string {
	var internal, hostIP, reserved []string
	var keys []string
	for _, p := range ports {
		label := fmt.Sprintf("%d", p.Target)
		if p.Service != "" {
			label = fmt.Sprintf("%s:%d", p.Service, p.Target)
		}
		keys = append(keys, portKey(p))
		if spec.LoopbackHost(p.HostIP) {
			continue // 只绑定在本机地址的不导入，不对外开放
		}
		if p.HostIP != "" {
			hostIP = append(hostIP, fmt.Sprintf("%s (host_ip %s)", label, p.HostIP))
		}
		if kind, ok := internalPorts[p.Target]; ok && p.Published != "" {
			internal = append(internal, fmt.Sprintf("%s (%s)", label, kind))
		}
		if p.Published == "80" || p.Published == "443" {
			reserved = append(reserved, label+" → "+p.Published)
		}
	}
	var out []string
	if len(internal) > 0 {
		out = append(out, fmt.Sprintf("Ports of internal services are published: %s. Nagi imports ports as port mappings on the first deploy and opens them to the outside. Databases and caches need no ports: containers of the project reach them by name (db:5432). Remove these ports unless the user wants them reachable from outside.", strings.Join(internal, ", ")))
	}
	if len(hostIP) > 0 {
		out = append(out, fmt.Sprintf("host_ip is ignored for %s: the port mapping opens the port on all of the node's addresses. Only loopback addresses (127.0.0.1, ::1) keep a port unpublished.", strings.Join(hostIP, ", ")))
	}
	if len(reserved) > 0 {
		out = append(out, fmt.Sprintf("Published ports 80 and 443 are reserved for domains, so %s is skipped. Serve HTTP with save_route instead: with domains, or without them on another published_port.", strings.Join(reserved, ", ")))
	}
	if o.deployed && o.oldPorts != nil && !slices.Equal(slices.Sorted(slices.Values(keys)), slices.Sorted(slices.Values(o.oldPorts))) {
		out = append(out, "This container has been deployed, so its ports were already imported as port mappings: changed ports here are not imported again. Change public ports and domains with save_route.")
	}
	return out
}

// advise 读出一份 compose 里要提醒的内容，解析不了时为 nil（保存时的校验会报错）。一次调用里同一份 compose 只读一遍，
// 结果交给 composeWarnings、composePorts、statefulWarning 共用。
func advise(text string) *spec.ComposeAdvice {
	a, err := spec.AdviseCompose(text)
	if err != nil {
		return nil
	}
	return &a
}

// composeWarnings 返回一份 compose（advise 的结果）的提醒，没有时为 nil。compose 解析不了（a 为 nil）时不提醒。
func composeWarnings(a *spec.ComposeAdvice, o adviceOpts) []string {
	if a == nil {
		return nil
	}
	var out []string
	if !o.imported && len(a.Services) == 1 {
		out = append(out, fmt.Sprintf("The compose file has only one service (%s). Unless the user asked for a compose file, a single service belongs in the image form: add_container with image, env (env_secret for secrets) and volumes, without a compose file.", a.Services[0]))
	}
	if len(a.LiteralSecrets) > 0 {
		var parts, names []string
		for _, svc := range a.Services {
			if list := a.LiteralSecrets[svc]; len(list) > 0 {
				parts = append(parts, svc+": "+strings.Join(list, ", "))
				for _, n := range list {
					if !slices.Contains(names, n) {
						names = append(names, n)
					}
				}
			}
		}
		out = append(out, fmt.Sprintf("The compose file hard-codes secrets (%s). Move them to Nagi variables: write NAME: ${NAME} in the compose file (or only the password in a connection string: postgres://app:${DB_PASSWORD}@db:5432/app), set the value with env and mark %s as secret with env_secret, or use update_shared_env when other containers of the project use it too.", strings.Join(parts, "; "), strings.Join(names, ", ")))
	}
	if !o.imported && len(a.EnvFiles) > 0 {
		var parts []string
		for _, svc := range a.Services {
			if files := a.EnvFiles[svc]; len(files) > 0 {
				parts = append(parts, svc+": "+strings.Join(files, ", "))
			}
		}
		out = append(out, fmt.Sprintf("The compose file reads env_file (%s). Nagi does not manage these files. Remove env_file and put the values in Nagi variables (env and env_secret, referenced as ${NAME}, or service_env): they are stored on the panel, versioned with the container and can be marked secret.", strings.Join(parts, "; ")))
	}
	if len(a.HostPaths) > 0 {
		var parts []string
		for _, h := range a.HostPaths {
			if h.Volume != "" {
				parts = append(parts, fmt.Sprintf("volume %s: %s", h.Volume, h.Source))
			} else {
				parts = append(parts, h.Service+": "+h.Source)
			}
		}
		if o.imported {
			out = append(out, fmt.Sprintf("The compose file mounts host paths outside the imported directory (%s). Data there is not included in snapshots, which can only cover subdirectories of the imported directory (backup_paths).", strings.Join(parts, "; ")))
		} else {
			out = append(out, fmt.Sprintf("The compose file mounts host paths outside the container directory (%s). Data there is not included in snapshots, backups or migrations and does not move with failover. Unless the user wants this host path, use a relative path (./name, stored in the container's data/) or a named volume (stored in its volumes/).", strings.Join(parts, "; ")))
		}
	}
	if len(a.Build) > 0 {
		out = append(out, fmt.Sprintf("%s uses build: nodes do not build images. Build the image in CI, push it to a registry and use image (log in to a private registry with login_registry).", strings.Join(a.Build, ", ")))
	}
	if len(a.ContainerName) > 0 {
		out = append(out, fmt.Sprintf("%s sets container_name. Remove it: Nagi names the containers itself, and a fixed name can clash with other projects on the node.", strings.Join(a.ContainerName, ", ")))
	}
	if len(a.HostNetwork) > 0 {
		out = append(out, fmt.Sprintf("%s uses network_mode: host: it bypasses Caddy, the project network and outbound limits, and its ports are open on the node directly. Remove it unless the user needs it.", strings.Join(a.HostNetwork, ", ")))
	}
	if len(a.ExternalVolumes) > 0 {
		out = append(out, fmt.Sprintf("External volumes (%s) are not in the container directory: snapshots, backups and migrations do not include them. Use named volumes that Nagi manages.", strings.Join(a.ExternalVolumes, ", ")))
	}
	if len(a.ExternalNetworks) > 0 {
		out = append(out, fmt.Sprintf("External networks (%s) connect to networks outside the project. To reach another project's containers, link the projects with connect_project and use <container>.<network name>.", strings.Join(a.ExternalNetworks, ", ")))
	}
	out = append(out, portWarnings(a.Ports, o)...)
	var values []labeled
	for _, svc := range a.Services {
		for _, k := range slices.Sorted(maps.Keys(a.EnvValues[svc])) {
			values = append(values, labeled{svc + ": " + k, a.EnvValues[svc][k]})
		}
		for _, h := range a.ExtraHosts[svc] {
			values = append(values, labeled{svc + ": extra_hosts", h})
		}
	}
	out = append(out, addressWarnings(values)...)
	var tags []string
	for _, svc := range a.DataServices {
		if img := a.Images[svc]; floatingTag(img) {
			tags = append(tags, svc+": "+img)
		}
	}
	if len(tags) > 0 {
		out = append(out, imageTagWarning(strings.Join(tags, ", ")))
	}
	return out
}

// formPortWarnings 检查镜像表单的端口。它的环境变量本来就是 Nagi 的变量，不用提醒。
func formPortWarnings(f *nagiv1.ImageForm, o adviceOpts) []string {
	var ports []spec.PortUse
	for _, p := range f.GetPorts() {
		ports = append(ports, spec.PortUse{Target: p.GetTarget(), Published: fmt.Sprint(p.GetPublished())})
	}
	return portWarnings(ports, o)
}

// formTagWarning：有数据的镜像表单容器没有固定镜像版本。
func formTagWarning(cfg *nagiv1.ContainerConfig) string {
	f := cfg.GetForm()
	if (len(f.GetVolumes()) > 0 || cfg.GetStateful() == nagiv1.Stateful_STATEFUL_YES) && floatingTag(f.GetImage()) {
		return imageTagWarning(f.GetImage())
	}
	return ""
}

// formPorts 是镜像表单端口的 portKey，和 portWarnings 里算的一样。
func formPorts(f *nagiv1.ImageForm) []string {
	out := []string{}
	for _, p := range f.GetPorts() {
		out = append(out, portKey(spec.PortUse{Target: p.GetTarget(), Published: fmt.Sprint(p.GetPublished())}))
	}
	return out
}

// composePorts 是 compose（advise 的结果）端口的 portKey；解析不了时为 nil（不比较）。
func composePorts(a *spec.ComposeAdvice) []string {
	if a == nil {
		return nil
	}
	out := []string{}
	for _, p := range a.Ports {
		out = append(out, portKey(p))
	}
	return out
}

// floatingTag 判断镜像有没有固定版本：没写标签、标签是 latest 的不算固定；写了摘要的算；用了变量的不知道，不算。
func floatingTag(image string) bool {
	if image == "" || strings.Contains(image, "$") || strings.Contains(image, "@") {
		return false
	}
	name := image[strings.LastIndexByte(image, '/')+1:]
	_, tag, hasTag := strings.Cut(name, ":")
	return !hasTag || tag == "latest"
}

func imageTagWarning(images string) string {
	return fmt.Sprintf("Containers with data should pin a major version, not latest or no tag (%s): an image update can bring a new major version that cannot read the existing data. Use a tag such as postgres:16.", images)
}

// statefulWarning：有数据（持久化目录、命名卷、保留路径）的容器却设成了无状态。a 是容器组的 compose 交给 advise 的结果。
func statefulWarning(cfg *nagiv1.ContainerConfig, a *spec.ComposeAdvice) string {
	if cfg.GetStateful() != nagiv1.Stateful_STATEFUL_NO {
		return ""
	}
	data := len(cfg.GetPreserve()) > 0 || len(cfg.GetForm().GetVolumes()) > 0
	if cfg.GetForm() == nil && a != nil && len(a.DataServices) > 0 {
		data = true
	}
	if !data {
		return ""
	}
	return "stateful is no although the container keeps data (persisted paths, named volumes or preserved paths). A stateless container is not paused for snapshots, gets no predeploy snapshot and is taken over without confirmation on failover. Use auto (the default) or yes for databases and other containers with data."
}

type labeled struct{ label, value string }

var (
	// nodeHostRe 是指向节点本机的写法：Docker 的宿主机名、host-gateway、默认网桥的网关。
	nodeHostRe = regexp.MustCompile(`(?i)host\.docker\.internal|host-gateway|\b172\.17\.0\.1\b`)
	// privateIPRe 是私网 IPv4 地址（容器 IP、局域网）。
	privateIPRe = regexp.MustCompile(`\b(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`)
	// localhostRe 是连接串里的 localhost：在容器里指的是容器自己。
	localhostRe = regexp.MustCompile(`(?i)(://|@)(localhost|127\.0\.0\.1)\b|\b(localhost|127\.0\.0\.1):\d+`)
)

// addressWarnings 检查变量值和 extra_hosts 里能看到的地址（${变量} 的值看不到，只查写出来的部分）。只列名字，不列值。
func addressWarnings(values []labeled) []string {
	var node, private, local []string
	for _, v := range values {
		switch {
		case nodeHostRe.MatchString(v.value):
			node = append(node, v.label)
		case privateIPRe.MatchString(v.value):
			private = append(private, v.label)
		case localhostRe.MatchString(v.value):
			local = append(local, v.label)
		}
	}
	var out []string
	if len(node) > 0 {
		out = append(out, fmt.Sprintf("%s point at the node itself (host.docker.internal, host-gateway or 172.17.0.1). New projects block connections from containers to the node and the LAN. Run the service the app needs (e.g. the database) as a container in the project and use its name, or ask an administrator to allow the address in the project's outbound settings.", strings.Join(node, ", ")))
	}
	if len(private) > 0 {
		out = append(out, fmt.Sprintf("%s contain private IP addresses. Containers reach each other by name, not by IP: <container> (db:5432) for a single container, <service>.<group> (db.umami) for a service in a group; container IPs change. New projects also block connections to LAN addresses: for a machine on the LAN, ask an administrator to allow it in the project's outbound settings.", strings.Join(private, ", ")))
	}
	if len(local) > 0 {
		out = append(out, fmt.Sprintf("%s use localhost, which inside a container is the container itself. Use the other container's name (db:5432).", strings.Join(local, ", ")))
	}
	return out
}

// configWarnings 是添加容器时对整份配置的提醒。env、serviceEnv 是这次设置的变量，查里面的地址。
func configWarnings(cfg *nagiv1.ContainerConfig, env map[string]string, serviceEnv map[string]map[string]string) []string {
	var out []string
	var a *spec.ComposeAdvice
	if f := cfg.GetForm(); f != nil {
		out = append(out, formPortWarnings(f, adviceOpts{})...)
		if w := formTagWarning(cfg); w != "" {
			out = append(out, w)
		}
	} else {
		a = advise(cfg.GetCompose())
		out = append(out, composeWarnings(a, adviceOpts{imported: cfg.GetProjectDir() != ""})...)
	}
	values := envLabels("env: ", env)
	for _, svc := range slices.Sorted(maps.Keys(serviceEnv)) {
		values = append(values, envLabels(svc+": ", serviceEnv[svc])...)
	}
	out = append(out, addressWarnings(values)...)
	if w := statefulWarning(cfg, a); w != "" {
		out = append(out, w)
	}
	return out
}

// envLabels 把这次设置的变量（名字 → 值）变成 addressWarnings 的输入。
func envLabels(prefix string, env map[string]string) []labeled {
	var out []labeled
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, labeled{prefix + k, env[k]})
	}
	return out
}

// envFileWarning 提醒 agent 不要用 .env 文件配置容器：compose 的变量来自 Nagi，容器目录里的 .env 由节点生成，
// 写进卷里的 .env 不会传给 compose。不是 .env 文件时返回空。
func envFileWarning(p string) string {
	name := path.Base(p)
	if name != ".env" && !strings.HasPrefix(name, ".env.") && !strings.HasSuffix(name, ".env") {
		return ""
	}
	return "This is a .env file. Nagi does not pass it to compose: variables come from Nagi (env and env_secret of the container, or update_shared_env for the project). Unless the app itself reads this file, set the values as Nagi variables instead: they are stored on the panel, versioned with the container and can be marked secret."
}

var (
	// dockerManageRe 是直接改动容器的 docker 命令：绕过 Nagi 的草稿、版本和顶替，下次部署又被改回去。
	dockerManageRe = regexp.MustCompile(`\bdocker(\s+compose|-compose)\b[^;&|\n]*\b(up|down|stop|start|restart|rm|kill|create|run|pause)\b|\bdocker\s+(container\s+)?(run|rm|stop|start|restart|kill|create|pause|update)\b`)
	dockerLoginRe  = regexp.MustCompile(`\bdocker\s+login\b`)
)

// nodeExecWarnings 是节点命令的提醒，放在预览里给 agent 和用户看。
func nodeExecWarnings(command string) []string {
	var out []string
	if dockerManageRe.MatchString(command) {
		out = append(out, "This command runs docker directly. Containers that Nagi manages are changed with update_container, deploy and control_container: docker run or docker compose up/stop/rm on the node bypasses drafts, versions and failover, and the next deploy undoes it.")
	}
	if dockerLoginRe.MatchString(command) {
		out = append(out, "Log in to private registries with login_registry instead of docker login on the node: Nagi passes the project's registry logins to every node it deploys to.")
	}
	return out
}

// generatedFiles 是节点在容器目录里按配置生成、每次部署都重写的文件。
var generatedFiles = []string{"compose.yml", "override.yml", ".env", "shared.env", "format"}

// nodeFileWarning：节点文件写在项目根目录（容器目录）里时提醒，放在预览里。root 是节点的项目根目录。
func nodeFileWarning(p, root string) string {
	root = path.Clean(root)
	if root == "." || root == "/" || p != root && !strings.HasPrefix(p, root+"/") {
		return ""
	}
	w := fmt.Sprintf("%s is under the node's projects root %s, where the container directories are.", p, root)
	if slices.Contains(generatedFiles, path.Base(p)) || path.Base(path.Dir(p)) == "env" {
		w += " Nagi rewrites compose.yml, override.yml, .env, shared.env and env/ on every deploy, so this change is lost."
	}
	return w + " Changes here bypass drafts, versions and failover: change the configuration with update_container and files in volumes with write_file."
}
