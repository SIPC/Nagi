package spec

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"gopkg.in/yaml.v3"
)

// compose 里能让容器碰到节点本身的写法。容器以特权运行或者挂载了主机目录，就等于拿到了节点的 root，
// 所以这些配置只有管理员能保存（见 docs/node-access.md）。按"读不懂就当作有特权"判断：这里的目的是拦住，不是解析。

// privilegedFields 是出现了就算有特权的服务字段（值不是空、不是 false）。
var privilegedFields = []string{
	"privileged", "cap_add", "devices", "device_cgroup_rules", "security_opt", "cgroup_parent", "credential_spec",
	"runtime", "volumes_from", "build", "extends",
}

// hostModeFields 是能和别处共用命名空间的服务字段：设为 host 时和节点共用，设为 container:<名字> 时和节点上任意一个容器共用
// （包括别的项目的容器，能进它的网络、看它的进程），network_mode 写网络名时直接接进那个网络。
// 所以只放行 safeModes 和指向同一个 compose 里的服务的 service:<服务>，其他的都算有特权。
var hostModeFields = []string{"network_mode", "pid", "ipc", "uts", "userns_mode", "cgroup"}

// safeModes 是 hostModeFields 里不碰别处的值。
var safeModes = []string{"none", "bridge", "default", "private", "shareable"}

// PrivilegeReasons 返回配置里只有管理员能保存的内容，例如 "web: privileged"。没有时为空。
// dynamic 表示其中有写法用了变量：变量的值能改变它的含义，所以有特权的配置里，不是管理员也不能改变量。
func PrivilegeReasons(cfg *nagiv1.ContainerConfig, container string) (reasons []string, dynamic bool) {
	text, err := ComposeText(cfg, container)
	if err != nil {
		return []string{"compose"}, false
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return []string{"compose"}, false
	}
	add := func(dyn bool, format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
		dynamic = dynamic || dyn
	}
	if doc["include"] != nil {
		add(false, "include")
	}
	services, ok := doc["services"].(map[string]any)
	if !ok && doc["services"] != nil {
		add(false, "services")
	}
	for name, raw := range services {
		svc, ok := raw.(map[string]any)
		if !ok {
			add(false, "%s", name)
			continue
		}
		checkService(name, svc, services, add)
	}
	checkTopLevel(doc, add)
	// 现有目录：compose 在节点上的任意目录里运行，相对路径都指向那里。
	if cfg.GetProjectDir() != "" {
		add(false, "project_dir")
	}
	if p := cfg.GetBackupPaths(); len(p) > 0 {
		add(false, "backup_paths: %s", strings.Join(p, ", "))
	}
	sort.Strings(reasons)
	return slices.Compact(reasons), dynamic
}

func checkService(name string, svc map[string]any, services map[string]any, add func(bool, string, ...any)) {
	for _, field := range privilegedFields {
		v, ok := svc[field]
		if !ok || v == nil || v == false {
			continue
		}
		add(hasVar(v), "%s: %s", name, field)
	}
	for _, field := range hostModeFields {
		v, ok := svc[field]
		if !ok || v == nil {
			continue
		}
		s, isStr := v.(string)
		other, isService := strings.CutPrefix(s, "service:")
		_, sameCompose := services[other]
		switch {
		case !isStr, strings.Contains(s, "$"):
			add(hasVar(v), "%s: %s", name, field)
		case s == "", slices.Contains(safeModes, s), isService && sameCompose:
		default:
			add(false, "%s: %s", name, field)
		}
	}
	for _, v := range listOf(svc["env_file"]) {
		p := v
		if m, ok := v.(map[string]any); ok {
			p = m["path"]
		}
		if s, ok := p.(string); !ok || !safeRelative(s) {
			add(hasVar(p), "%s: env_file", name)
		}
	}
	if v, ok := svc["volumes"]; ok && v != nil {
		list, isList := v.([]any)
		if !isList {
			add(hasVar(v), "%s: volumes", name)
		}
		for _, m := range list {
			if !safeMount(m) {
				add(hasVar(m), "%s: volumes", name)
			}
		}
	}
}

func checkTopLevel(doc map[string]any, add func(bool, string, ...any)) {
	for _, section := range []string{"secrets", "configs"} {
		m, _ := doc[section].(map[string]any)
		for name, raw := range m {
			e, _ := raw.(map[string]any)
			if f, has := e["file"]; has {
				if s, ok := f.(string); !ok || !safeRelative(s) {
					add(hasVar(f), "%s: %s", section, name)
				}
			}
		}
	}
	// 写了 name 的卷和网络不加 compose 项目的前缀，节点上已经有同名的（别的项目的卷、网络）就直接用它。
	volumes, _ := doc["volumes"].(map[string]any)
	for name, raw := range volumes {
		e, _ := raw.(map[string]any)
		for _, field := range []string{"driver", "driver_opts", "external", "name"} {
			if v, has := e[field]; has && v != nil && v != false {
				add(hasVar(v), "volumes: %s", name)
			}
		}
	}
	networks, _ := doc["networks"].(map[string]any)
	for name, raw := range networks {
		e, _ := raw.(map[string]any)
		for _, field := range []string{"external", "name"} {
			if v, has := e[field]; has && v != nil && v != false {
				add(hasVar(v), "networks: %s", name)
			}
		}
	}
}

// safeMount 判断挂载是不是留在容器自己的目录里：匿名卷、命名卷（不带 driver，上面单独查）、容器目录里的相对路径。
func safeMount(m any) bool {
	switch v := m.(type) {
	case string:
		// 短写法 源:目标[:选项]。只有一段时是匿名卷。
		src, _, hasTarget := strings.Cut(v, ":")
		if !hasTarget {
			return !strings.Contains(v, "$")
		}
		return safeSource(src)
	case map[string]any:
		typ, _ := v["type"].(string)
		src, _ := v["source"].(string)
		switch typ {
		case "volume":
			return v["source"] == nil || safeSource(src)
		case "tmpfs":
			return true
		case "bind":
			return safeRelative(src)
		}
	}
	return false
}

// safeSource 判断短写法的来源：相对路径留在容器目录里；不以 . / ~ 开头的是命名卷。
func safeSource(src string) bool {
	if strings.HasPrefix(src, ".") {
		return safeRelative(src)
	}
	return src != "" && !strings.ContainsAny(src, "/\\~$")
}

// safeRelative 判断路径是不是留在容器目录里的相对路径。
func safeRelative(p string) bool {
	if p == "" || strings.ContainsAny(p, "~$\\") || path.IsAbs(p) {
		return false
	}
	c := path.Clean(p)
	return c != ".." && !strings.HasPrefix(c, "../")
}

func listOf(v any) []any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		return x
	}
	return []any{v}
}

// hasVar 判断值里有没有引用变量。
func hasVar(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, "$")
	case []any:
		return slices.ContainsFunc(x, hasVar)
	case map[string]any:
		for _, e := range x {
			if hasVar(e) {
				return true
			}
		}
	}
	return false
}
