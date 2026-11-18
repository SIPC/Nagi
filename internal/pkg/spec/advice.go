package spec

import (
	"cmp"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// compose 里能保存、但不建议的写法（见 docs/mcp.md#给-agent-的说明）：只用来提醒 agent，不影响保存。
// 和 PrivilegeReasons 一样用 composeModel 读出 compose 解析库看到的内容，不做变量替换，保持原文的写法。

// secretName 判断变量名像不像密钥。
var secretName = regexp.MustCompile(`(?i)(PASS|SECRET|TOKEN|KEY|CREDENTIAL|PRIVATE)`)

// LooksSecret 判断变量名像不像密钥（例如 POSTGRES_PASSWORD、API_TOKEN），像的建议标为密钥。
func LooksSecret(name string) bool { return secretName.MatchString(name) }

// HostPath 是挂载到容器目录以外的宿主机路径：Service 的挂载，或者顶层命名卷 Volume 的 driver_opts.device。
type HostPath struct {
	Service, Volume, Source string
}

// ComposeAdvice 是一份 compose 里不建议的写法。
type ComposeAdvice struct {
	// Services 是服务名，排好序。
	Services []string
	// LiteralEnv 是服务 environment 里写死了值（不是 ${变量} 引用、也不是只写名字从变量里取）的变量名。
	LiteralEnv map[string][]string
	// EnvFiles 是服务的 env_file 路径。
	EnvFiles map[string][]string
	// HostPaths 是容器目录以外的宿主机路径，按服务、卷名排好序。
	HostPaths []HostPath
	// Ports 是服务的 ports，按服务排好序。
	Ports []PortUse
	// EnvValues 是 environment 里写成字符串的值（包括带 ${变量} 的），用来检查地址；ExtraHosts 是 extra_hosts 的每一项。
	EnvValues  map[string]map[string]string
	ExtraHosts map[string][]string
	// Images 是服务的镜像。DataServices 是挂载了命名卷或容器目录里的相对路径、有数据的服务。
	Images       map[string]string
	DataServices []string
	// Build、ContainerName、HostNetwork 是用了 build、container_name、network_mode: host 的服务。
	Build, ContainerName, HostNetwork []string
	// ExternalVolumes、ExternalNetworks 是顶层声明为 external 的卷和网络。
	ExternalVolumes, ExternalNetworks []string
}

// PortUse 是 compose 里的一个端口：容器端口、对外端口（可能是范围或空）和 host_ip。
type PortUse struct {
	Service           string
	Target            uint32
	Published, HostIP string
}

// varRef 判断值里有没有引用变量（$$ 是转义的 $，不算）。
var varRef = regexp.MustCompile(`\$(\{|[A-Za-z_])`)

func hasVarRef(s string) bool { return varRef.MatchString(strings.ReplaceAll(s, "$$", "")) }

// systemPaths 是挂进容器的系统文件（时区、套接字、设备），不是数据，不提醒。
var systemPaths = []string{"/etc/localtime", "/etc/timezone", "/var/run", "/run", "/dev", "/sys", "/proc"}

func systemPath(p string) bool {
	c := path.Clean(p)
	return slices.ContainsFunc(systemPaths, func(s string) bool { return c == s || strings.HasPrefix(c, s+"/") })
}

// outsideHostPath 判断挂载的源路径是不是容器目录以外的宿主机路径：绝对路径、~、..（包括 Windows 的盘符和命名管道）。
// 用了变量的不知道指向哪里，不算。
func outsideHostPath(src string) bool {
	return src != "" && !strings.Contains(src, "$") && !safeRelative(src) && !systemPath(src)
}

// AdviseCompose 找出 compose 里不建议的写法。compose 解析不了时返回错误（保存时的校验会报出具体问题）。
func AdviseCompose(text string) (ComposeAdvice, error) {
	model, err := composeModel(text)
	if err != nil {
		return ComposeAdvice{}, err
	}
	out := ComposeAdvice{LiteralEnv: map[string][]string{}, EnvFiles: map[string][]string{}, EnvValues: map[string]map[string]string{},
		ExtraHosts: map[string][]string{}, Images: map[string]string{}}
	services, _ := model["services"].(map[string]any)
	for name, raw := range services {
		out.Services = append(out.Services, name)
		svc, _ := raw.(map[string]any)
		if names := literalEnv(svc["environment"]); len(names) > 0 {
			out.LiteralEnv[name] = names
		}
		if vals := envValues(svc["environment"]); len(vals) > 0 {
			out.EnvValues[name] = vals
		}
		for _, h := range extraHosts(svc["extra_hosts"]) {
			out.ExtraHosts[name] = append(out.ExtraHosts[name], h)
		}
		if img, ok := svc["image"].(string); ok {
			out.Images[name] = img
		}
		if svc["build"] != nil {
			out.Build = append(out.Build, name)
		}
		if svc["container_name"] != nil {
			out.ContainerName = append(out.ContainerName, name)
		}
		if svc["network_mode"] == "host" {
			out.HostNetwork = append(out.HostNetwork, name)
		}
		out.Ports = append(out.Ports, servicePorts(name, svc["ports"])...)
		for _, f := range listOf(svc["env_file"]) {
			p := f
			if m, ok := f.(map[string]any); ok {
				p = m["path"]
			}
			if s, ok := p.(string); ok && s != "" {
				out.EnvFiles[name] = append(out.EnvFiles[name], s)
			}
		}
		data := false
		for _, m := range listOf(svc["volumes"]) {
			v, _ := m.(map[string]any)
			typ, _ := v["type"].(string)
			src, _ := v["source"].(string)
			switch {
			case typ == "volume" && src != "", typ == "bind" && safeRelative(src):
				data = true
			case typ == "bind" && outsideHostPath(src):
				out.HostPaths = append(out.HostPaths, HostPath{Service: name, Source: src})
			}
		}
		if data {
			out.DataServices = append(out.DataServices, name)
		}
	}
	// 命名卷用 driver_opts 绑到宿主机目录（type: none, o: bind, device: /opt/pg），数据同样不在容器目录里。
	volumes, _ := model["volumes"].(map[string]any)
	for name, raw := range volumes {
		v, _ := raw.(map[string]any)
		opts, _ := v["driver_opts"].(map[string]any)
		if dev, ok := opts["device"].(string); ok && strings.HasPrefix(dev, "/") && outsideHostPath(dev) {
			out.HostPaths = append(out.HostPaths, HostPath{Volume: name, Source: dev})
		}
		if external(v) {
			out.ExternalVolumes = append(out.ExternalVolumes, name)
		}
	}
	networks, _ := model["networks"].(map[string]any)
	for name, raw := range networks {
		if v, _ := raw.(map[string]any); external(v) {
			out.ExternalNetworks = append(out.ExternalNetworks, name)
		}
	}
	for _, l := range [][]string{out.Services, out.DataServices, out.Build, out.ContainerName, out.HostNetwork, out.ExternalVolumes, out.ExternalNetworks} {
		sort.Strings(l)
	}
	slices.SortStableFunc(out.Ports, func(a, b PortUse) int { return strings.Compare(a.Service, b.Service) })
	slices.SortFunc(out.HostPaths, func(a, b HostPath) int {
		return cmp.Or(strings.Compare(a.Service, b.Service), strings.Compare(a.Volume, b.Volume), strings.Compare(a.Source, b.Source))
	})
	return out, nil
}

// external 判断顶层的卷或网络是不是 external（compose 的旧写法 external: {name: x} 也算）。
func external(v map[string]any) bool {
	e, ok := v["external"]
	return ok && e != nil && e != false
}

// servicePorts 读出服务的 ports。compose 解析库已经把短写法展开成 map；用了变量、解析不了的短写法保持原样，
// 这里再解析一次，还是不行就跳过。
func servicePorts(service string, ports any) []PortUse {
	var out []PortUse
	for _, raw := range listOf(ports) {
		switch p := raw.(type) {
		case map[string]any:
			target, _ := strconv.ParseUint(fmt.Sprint(p["target"]), 10, 32)
			published := ""
			if v := p["published"]; v != nil {
				published = fmt.Sprint(v)
			}
			host, _ := p["host_ip"].(string)
			out = append(out, PortUse{Service: service, Target: uint32(target), Published: published, HostIP: host})
		case string, int:
			parsed, err := types.ParsePortConfig(fmt.Sprint(p))
			if err != nil {
				continue
			}
			for _, c := range parsed {
				out = append(out, PortUse{Service: service, Target: c.Target, Published: c.Published, HostIP: c.HostIP})
			}
		}
	}
	return out
}

// envValues 返回 environment 里写成字符串的值。
func envValues(env any) map[string]string {
	out := map[string]string{}
	switch e := env.(type) {
	case map[string]any:
		for k, v := range e {
			if s, ok := v.(string); ok && s != "" {
				out[k] = s
			}
		}
	case []any:
		for _, item := range e {
			if s, ok := item.(string); ok {
				if k, v, has := strings.Cut(s, "="); has && v != "" {
					out[k] = v
				}
			}
		}
	}
	return out
}

// extraHosts 返回 extra_hosts 的每一项，写成 名字:地址（列表或 map 两种写法）。
func extraHosts(v any) []string {
	var out []string
	switch e := v.(type) {
	case map[string]any:
		for k, ip := range e {
			out = append(out, fmt.Sprintf("%s:%v", k, ip))
		}
	case []any:
		for _, item := range e {
			out = append(out, fmt.Sprint(item))
		}
	}
	sort.Strings(out)
	return out
}

// literalEnv 返回 environment 里写死了值的变量名，排好序。environment 可以是 map（值为空表示从变量里取），
// 也可以是 NAME=值 的列表（只写 NAME 表示从变量里取）。
func literalEnv(env any) []string {
	var names []string
	switch e := env.(type) {
	case map[string]any:
		for k, v := range e {
			if s, ok := v.(string); v == nil || ok && (s == "" || hasVarRef(s)) {
				continue
			}
			names = append(names, k)
		}
	case []any:
		for _, item := range e {
			s, ok := item.(string)
			if !ok {
				continue
			}
			k, v, hasValue := strings.Cut(s, "=")
			if hasValue && v != "" && !hasVarRef(v) {
				names = append(names, k)
			}
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}
