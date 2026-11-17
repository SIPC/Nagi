package spec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// compose 里能让容器碰到节点本身的写法。容器以特权运行或者挂载了主机目录，就等于拿到了节点的 root，
// 所以这些配置只有管理员能保存（见 docs/node-access.md）。按"读不懂就当作有特权"判断：这里的目的是拦住，不是解析。

// privilegedFields 是出现了就算有特权的服务字段（值不是空、不是 false）。
var privilegedFields = []string{
	"privileged", "cap_add", "devices", "device_cgroup_rules", "security_opt", "cgroup_parent", "credential_spec",
	"runtime", "volumes_from", "build", "extends",
	// provider：compose 在节点上以 root 运行一个现有的程序来提供这个服务。
	"provider",
}

// safeServiceFields 是服务里不碰节点的字段，不用再看值。服务字段按白名单判断：不在这里、也不在下面单独检查的字段
// （privilegedFields、hookFields、hostModeFields、env_file、label_file、volumes、logging、deploy）一律算有特权，
// x- 开头的扩展字段除外。compose 还在不断增加字段（例如 use_api_socket 把节点的 docker.sock 挂进容器），
// 节点上的版本也不由 Nagi 决定，按「已知危险的字段」逐个列出总会漏掉新字段。
var safeServiceFields = []string{
	"image", "command", "entrypoint", "environment", "ports", "expose", "healthcheck", "restart", "depends_on",
	"labels", "working_dir", "user", "group_add", "hostname", "domainname",
	"networks", "dns", "dns_opt", "dns_search", "extra_hosts", "links", "mac_address",
	"configs", "secrets", "tmpfs", "read_only", "init", "tty", "stdin_open", "attach",
	"stop_signal", "stop_grace_period", "platform", "pull_policy", "pull_refresh_after", "profiles", "scale",
	"cpus", "cpu_count", "cpu_percent", "cpu_shares", "cpu_period", "cpu_quota", "cpuset",
	"mem_limit", "mem_reservation", "mem_swappiness", "memswap_limit", "shm_size", "pids_limit",
	"sysctls", "isolation",
}

// safeTopLevel 是顶层不碰节点的段（volumes、networks、secrets、configs 另外检查值）。其他顶层段一律算有特权，
// x- 开头的除外（例如 models、include）。
var safeTopLevel = []string{"services", "volumes", "networks", "secrets", "configs", "name", "version"}

// safeLogDrivers 是在容器自己那里写日志的驱动：syslog、gelf、fluentd 这类由 dockerd 在节点的网络里发出去，
// 能绕过出站限制访问节点所在的内网。
// 不写 driver 时用 dockerd 的默认驱动，由节点的 daemon.json 决定，所以不写 driver 时不能带 options。
var safeLogDrivers = []string{"json-file", "local", "none"}

// safeLogOptions 是 json-file、local 驱动的日志选项里不碰节点的。
var safeLogOptions = []string{"max-size", "max-file", "compress", "labels", "env", "tag", "labels-regex", "env-regex"}

// unsafeUlimits 是能让容器里的进程抢占整个节点的 ulimit：实时调度、调高优先级、锁住大量内存。
var unsafeUlimits = []string{"rtprio", "rttime", "nice", "memlock"}

// hookFields 是容器的生命周期钩子：在容器里执行命令，可以单独设 privileged，以特权执行。
var hookFields = []string{"post_start", "pre_stop"}

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
	if r := yamlShapeReason(text); r != "" {
		return []string{r}, false
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return []string{"compose"}, false
	}
	add := func(dyn bool, format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
		dynamic = dynamic || dyn
	}
	checkDoc(doc, add)
	// 再按 compose 自己的解析结果检查一遍：合并键、锚点、标签这类写法，compose 和这里用的 YAML 库理解可能不同，
	// 检查要看到节点上 compose 实际读到的内容。compose 解析不了的算有特权。
	model, err := composeModel(text)
	if err != nil {
		add(false, "compose")
	} else {
		checkDoc(model, add)
	}
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

// checkDoc 检查一份解析出来的 compose（顶层 map）。
func checkDoc(doc map[string]any, add func(bool, string, ...any)) {
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
	checkNestedBinds(doc, services, add)
}

// composeModel 用 compose 的解析库（和节点上部署时同一套规则）把 compose 读成 map：合并多个文档、展开锚点和合并键、
// 按 !reset / !override 处理。不做变量替换、不补默认值、不解析相对路径，保持和原文一样的写法，交给 checkDoc。
func composeModel(text string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return loader.LoadModelWithContext(ctx, types.ConfigDetails{
		WorkingDir:  "/nagi-privilege-check",
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yml", Content: []byte(text)}},
		Environment: types.Mapping{},
	}, func(o *loader.Options) {
		o.SetProjectName("nagi-privilege-check", true)
		o.SkipInterpolation = true
		o.SkipValidation = true
		o.SkipNormalization = true
		o.SkipConsistencyCheck = true
		o.SkipResolveEnvironment = true
		o.SkipDefaultValues = true
		o.SkipInclude = true
		o.SkipExtends = true
		o.ResolvePaths = false
	})
}

func checkService(name string, svc map[string]any, services map[string]any, add func(bool, string, ...any)) {
	checked := []string{"env_file", "label_file", "volumes", "logging", "deploy", "ulimits", "oom_score_adj", "oom_kill_disable"}
	for field, v := range svc {
		switch {
		case strings.HasPrefix(field, "x-"), slices.Contains(safeServiceFields, field), slices.Contains(checked, field),
			slices.Contains(privilegedFields, field), slices.Contains(hookFields, field), slices.Contains(hostModeFields, field):
		default:
			add(hasVar(v), "%s: %s", name, field)
		}
	}
	// label_file 和 env_file 一样由 compose 在节点上以 root 读取，只能是容器目录里的相对路径。
	for _, v := range listOf(svc["label_file"]) {
		if s, ok := v.(string); !ok || !safeRelative(s) {
			add(hasVar(v), "%s: label_file", name)
		}
	}
	if v, ok := svc["logging"]; ok && v != nil {
		if !safeLogging(v) {
			add(hasVar(v), "%s: logging", name)
		}
	}
	// 内存不够时内核先杀别的进程（节点进程、dockerd、别的项目的容器）。
	if v, ok := svc["oom_score_adj"]; ok && v != nil {
		if n, isInt := v.(int); !isInt || n < 0 {
			add(hasVar(v), "%s: oom_score_adj", name)
		}
	}
	if v, ok := svc["oom_kill_disable"]; ok && v != nil && v != false {
		add(hasVar(v), "%s: oom_kill_disable", name)
	}
	if v, ok := svc["ulimits"]; ok && v != nil {
		m, isMap := v.(map[string]any)
		if !isMap {
			add(hasVar(v), "%s: ulimits", name)
		}
		for k, e := range m {
			if slices.Contains(unsafeUlimits, k) {
				add(hasVar(e), "%s: ulimits.%s", name, k)
			}
		}
	}
	// deploy 里只有设备（GPU 这类）碰节点：resources.reservations.devices。
	if v, ok := svc["deploy"]; ok && v != nil && containsKey(v, "devices") {
		add(hasVar(v), "%s: deploy.devices", name)
	}
	for _, field := range privilegedFields {
		v, ok := svc[field]
		if !ok || v == nil || v == false {
			continue
		}
		add(hasVar(v), "%s: %s", name, field)
	}
	// 钩子带了 privileged 就以特权执行；写法读不懂（不是列表、元素不是对象）时按有特权算。
	for _, field := range hookFields {
		v, ok := svc[field]
		if !ok || v == nil {
			continue
		}
		list, isList := v.([]any)
		if !isList {
			add(hasVar(v), "%s: %s", name, field)
			continue
		}
		for _, h := range list {
			hook, isMap := h.(map[string]any)
			if !isMap {
				add(hasVar(h), "%s: %s", name, field)
				continue
			}
			if p, has := hook["privileged"]; has && p != nil && p != false {
				add(hasVar(p), "%s: %s.privileged", name, field)
			}
		}
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

// safeLogging：驱动是 safeLogDrivers 之一，选项只有 safeLogOptions。
func safeLogging(v any) bool {
	m, isMap := v.(map[string]any)
	if !isMap {
		return false
	}
	for k := range m {
		if k != "driver" && k != "options" {
			return false
		}
	}
	driver, _ := m["driver"].(string)
	if !slices.Contains(safeLogDrivers, driver) {
		return false
	}
	if o, has := m["options"]; has && o != nil {
		opts, isMap := o.(map[string]any)
		if !isMap {
			return false
		}
		for k := range opts {
			if !slices.Contains(safeLogOptions, k) {
				return false
			}
		}
	}
	return true
}

// containsKey 判断 v 里（任意层）有没有名为 key 的字段。
func containsKey(v any, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == key || containsKey(e, key) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if containsKey(e, key) {
				return true
			}
		}
	}
	return false
}

func checkTopLevel(doc map[string]any, add func(bool, string, ...any)) {
	for section, v := range doc {
		if section != "include" && !strings.HasPrefix(section, "x-") && !slices.Contains(safeTopLevel, section) {
			add(hasVar(v), "%s", section)
		}
	}
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
	// 网络的驱动和参数也算：macvlan、ipvlan 让容器直接接入节点所在的局域网，流量不经过宿主机的
	// forward 链，出站限制管不到；driver_opts 能指定宿主机的网桥（com.docker.network.bridge.name）；
	// ipam 能指定和局域网重叠的网段。只有默认的 bridge 驱动、不带参数的网络不算。
	networks, _ := doc["networks"].(map[string]any)
	for name, raw := range networks {
		e, _ := raw.(map[string]any)
		for _, field := range []string{"external", "name", "driver_opts", "ipam"} {
			if v, has := e[field]; has && v != nil && v != false {
				add(hasVar(v), "networks: %s", name)
			}
		}
		if v, has := e["driver"]; has && v != nil && v != "bridge" {
			add(hasVar(v), "networks: %s", name)
		}
	}
}

// checkNestedBinds：一个挂载的源路径在另一个挂载（或 secrets、configs 的文件）的上级目录时，
// 能写上级目录的容器可以把下级换成指向宿主机的符号链接，Docker 每次启动容器都会重新解析源路径，
// 下次启动就把宿主机的目录挂进去。部署前的检查管不到之后的重启，所以这种写法只有管理员能保存。
func checkNestedBinds(doc map[string]any, services map[string]any, add func(bool, string, ...any)) {
	type bind struct{ owner, path string }
	var binds, files []bind
	for name, raw := range services {
		svc, _ := raw.(map[string]any)
		for _, m := range listOf(svc["volumes"]) {
			if p, ok := relativeBind(m); ok {
				binds = append(binds, bind{name, p})
			}
		}
	}
	for _, section := range []string{"secrets", "configs"} {
		m, _ := doc[section].(map[string]any)
		for name, raw := range m {
			e, _ := raw.(map[string]any)
			if f, ok := e["file"].(string); ok && safeRelative(f) {
				files = append(files, bind{section + ": " + name, path.Clean(f)})
			}
		}
	}
	for _, parent := range binds {
		for _, child := range append(binds, files...) {
			if child.path != parent.path && (parent.path == "." || strings.HasPrefix(child.path, parent.path+"/")) {
				add(false, "%s: volumes: %s, %s", parent.owner, parent.path, child.path)
			}
		}
	}
}

// relativeBind 返回挂载里容器目录下的相对源路径（已规范化）。
func relativeBind(m any) (string, bool) {
	src := ""
	switch v := m.(type) {
	case string:
		s, _, hasTarget := strings.Cut(v, ":")
		if !hasTarget || !strings.HasPrefix(s, ".") {
			return "", false
		}
		src = s
	case map[string]any:
		if typ, _ := v["type"].(string); typ != "bind" {
			return "", false
		}
		src, _ = v["source"].(string)
	}
	if !safeRelative(src) {
		return "", false
	}
	return path.Clean(src), true
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

// yamlShapeReason：普通的 YAML 解析和 compose 理解不一样的写法一律算有特权（读不懂就当作有特权）：
//   - 多个文档（`---` 分隔）：compose 会把后面的文档合并进来；
//   - 显式写了标签的（`!reset`、`!override`，以及 `!!merge` 这类标准标签）：compose 按标签处理，例如把带 `!!merge`
//     的普通键当作合并键展开，这里的 YAML 库却当作普通键；正常的 compose 用不到显式标签；
//   - `%TAG` 这类指令：能把自定义的前缀展开成标准标签。
//
// 不是合法 YAML 的交给后面的解析报错。
func yamlShapeReason(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "%") {
			return "compose: YAML directive"
		}
	}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	// 下面的检查只读第一个文档，所以除了第一个，后面的文档都得是空的（例如末尾多一个 ---）。
	for i := 0; ; i++ {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "compose"
		}
		if emptyDoc(&n) {
			continue
		}
		if i > 0 {
			return "compose: multiple YAML documents"
		}
		if customTag(&n) {
			return "compose: YAML tag"
		}
	}
	return ""
}

func emptyDoc(n *yaml.Node) bool {
	if n.Kind == 0 || n.Kind == yaml.DocumentNode && len(n.Content) == 0 {
		return true
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		c := n.Content[0]
		return c.Kind == yaml.ScalarNode && c.Tag == "!!null" && len(c.Content) == 0
	}
	return false
}

func customTag(n *yaml.Node) bool {
	if n.Style&yaml.TaggedStyle != 0 || n.Tag != "" && !strings.HasPrefix(n.Tag, "!!") {
		return true
	}
	for _, c := range n.Content {
		if customTag(c) {
			return true
		}
	}
	return false
}
