// Package spec 是容器配置的规则，面板和节点共用：把镜像表单转成 compose、解析和校验配置、
// 判断有没有状态、端口映射的格式。面板保存配置前用它校验，节点部署时用它解析。
package spec

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// ErrInvalid 是参数错误：errors.Is(err, ErrInvalid) 能识别 Invalid 返回的错误。
var ErrInvalid = errors.New("参数无效")

type invalidError struct{ err error }

func (e invalidError) Error() string        { return e.err.Error() }
func (e invalidError) Unwrap() error        { return e.err }
func (e invalidError) Is(target error) bool { return target == ErrInvalid }

// Invalid 返回一个参数错误，消息只包含具体原因。按请求的语言显示（见 i18n）。
func Invalid(format string, args ...any) error {
	return invalidError{err: i18n.Errorf(format, args...)}
}

var (
	// NameRe 是容器名、网络名的格式：小写字母、数字和 -，不以 - 开头或结尾，最长 32 个字符。
	NameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// varRefRe 匹配 compose 里引用变量的写法：$VAR、${VAR}、${VAR:-默认值} 等。
	varRefRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
	// scpURLRe 是 scp 风格的 SSH 地址：用户@主机:路径。主机里不能有 /，避免把 /srv/repo@x 这样的本地路径当成 SSH 地址
	// （go-git 会把本地路径当作本地仓库，拉取节点上的任意仓库）。
	scpURLRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s]+$`)
)

// CheckContainerName 检查容器名（容器组的名字也一样）。
func CheckContainerName(name string) error {
	if !NameRe.MatchString(name) {
		return Invalid("容器名只能用小写字母、数字和 -，不以 - 开头或结尾，不超过 32 个字符")
	}
	return nil
}

// CheckNetworkName 检查项目的网络名。
func CheckNetworkName(name string) error {
	if !NameRe.MatchString(name) {
		return Invalid("网络名只能用小写字母、数字和 -，不以 - 开头或结尾，不超过 32 个字符")
	}
	return nil
}

// IsSingle 判断容器是不是单个容器（用镜像表单创建）。
func IsSingle(cfg *nagiv1.ContainerConfig) bool { return cfg.GetForm() != nil }

// Kind 返回容器的种类：single（单个容器，镜像表单）或 group（容器组，一份 compose）。
func Kind(cfg *nagiv1.ContainerConfig) string {
	if IsSingle(cfg) {
		return "single"
	}
	return "group"
}

// ComposeText 返回容器配置对应的 compose 原文：单个容器由表单生成，服务名就是容器名。
func ComposeText(cfg *nagiv1.ContainerConfig, container string) (string, error) {
	if cfg.GetForm() != nil {
		return formCompose(cfg.GetForm(), container)
	}
	return cfg.GetCompose(), nil
}

// ServiceEnv 返回每个服务的环境变量。镜像表单的环境变量属于唯一的服务（服务名就是容器名）。
func ServiceEnv(cfg *nagiv1.ContainerConfig, container string) map[string][]*nagiv1.EnvVar {
	out := map[string][]*nagiv1.EnvVar{}
	for name, list := range cfg.GetServiceEnv() {
		if len(list.GetVars()) > 0 {
			out[name] = list.GetVars()
		}
	}
	if f := cfg.GetForm(); f != nil && len(f.GetEnv()) > 0 {
		out[container] = append(out[container], f.GetEnv()...)
	}
	return out
}

// escapeDollar 把命令里的 $ 写成 $$：表单里填的命令是给容器里的 shell 执行的（例如 echo $HOME），
// 不转义的话 compose 会先把 $HOME 当成自己的变量替换掉（通常变成空的）。
func escapeDollar(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// formCompose 把镜像表单转换成 compose。环境变量不写进 compose，而是放在服务级环境变量文件里，
// 这样导出的 compose 里不会带着密钥。
func formCompose(f *nagiv1.ImageForm, service string) (string, error) {
	image := strings.TrimSpace(f.GetImage())
	if image == "" {
		return "", Invalid("镜像不能为空")
	}
	svc := map[string]any{"image": image}
	if cmd := strings.TrimSpace(f.GetCommand()); cmd != "" {
		svc["command"] = escapeDollar(cmd)
	}
	var ports []string
	for _, p := range f.GetPorts() {
		if p.GetPublished() == 0 || p.GetTarget() == 0 || p.GetPublished() > 65535 || p.GetTarget() > 65535 {
			return "", Invalid("端口必须在 1 到 65535 之间")
		}
		proto := strings.ToLower(p.GetProtocol())
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			return "", Invalid("端口协议只能是 tcp 或 udp")
		}
		ports = append(ports, fmt.Sprintf("%d:%d/%s", p.GetPublished(), p.GetTarget(), proto))
	}
	if len(ports) > 0 {
		svc["ports"] = ports
	}

	var mounts []string
	volumes := map[string]any{}
	used := map[string]bool{}
	for _, target := range f.GetVolumes() {
		target = strings.TrimSpace(target)
		if !strings.HasPrefix(target, "/") {
			return "", Invalid("持久化目录必须是容器内的绝对路径：%s", target)
		}
		name := volumeName(target, used)
		volumes[name] = map[string]any{}
		mounts = append(mounts, name+":"+target)
	}
	if wd := strings.TrimSpace(f.GetWorkdir()); wd != "" {
		if !strings.HasPrefix(wd, "/") {
			return "", Invalid("工作目录必须是容器内的绝对路径")
		}
		// compose 的相对路径以容器的 data/ 为准，所以 "./" 就是 data/。
		mounts = append(mounts, "./:"+wd)
		svc["working_dir"] = wd
	}
	if len(mounts) > 0 {
		svc["volumes"] = mounts
	}
	if mb := f.GetMemoryMb(); mb > 0 {
		svc["mem_limit"] = fmt.Sprintf("%dm", mb)
	}
	if c := f.GetCpus(); c < 0 || c > 1024 {
		return "", Invalid("CPU 上限不对")
	} else if c > 0 {
		svc["cpus"] = c
	}
	if hc := strings.TrimSpace(f.GetHealthCommand()); hc != "" {
		interval := f.GetHealthIntervalSeconds()
		if interval == 0 {
			interval = 30
		}
		svc["healthcheck"] = map[string]any{
			"test":         []string{"CMD-SHELL", escapeDollar(hc)},
			"interval":     fmt.Sprintf("%ds", interval),
			"timeout":      fmt.Sprintf("%ds", min(interval, 10)),
			"retries":      3,
			"start_period": fmt.Sprintf("%ds", interval),
		}
	}

	doc := map[string]any{"services": map[string]any{service: svc}}
	if len(volumes) > 0 {
		doc["volumes"] = volumes
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return "# 由镜像表单生成\n" + string(out), nil
}

// volumeName 根据容器路径生成命名 volume 的名字，例如 /var/lib/redis → var_lib_redis。
func volumeName(target string, used map[string]bool) string {
	var b strings.Builder
	for _, r := range strings.Trim(target, "/") {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
	}
	name := strings.ToLower(strings.Trim(b.String(), "_"))
	if name == "" {
		name = "data"
	}
	base, n := name, 2
	for used[name] {
		name = fmt.Sprintf("%s_%d", base, n)
		n++
	}
	used[name] = true
	return name
}

// ComposeProjectName 是容器的 compose 项目名。
func ComposeProjectName(projectID, container string) string {
	return "nagi-" + projectID + "-" + container
}

// Validate 校验容器配置：compose 能解析、变量名合法、服务级变量对应的服务存在、git 地址有效、依赖的容器名合法。
// shared 是项目的共享变量，用来替换 compose 里的 ${变量}（同名时以容器自己的为准）。会规范 cfg 里的部分字段。
// dataDir 是容器的 data/ 目录（compose 里相对路径的根），面板上校验时用一个虚拟的路径。
func Validate(ctx context.Context, cfg *nagiv1.ContainerConfig, container string, shared []*nagiv1.EnvVar, dataDir, composeProject string) (*types.Project, error) {
	text, err := ComposeText(cfg, container)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, Invalid("compose 不能为空")
	}
	if err := CheckEnv(cfg.GetEnv()); err != nil {
		return nil, err
	}
	for _, vars := range ServiceEnv(cfg, container) {
		if err := CheckEnv(vars); err != nil {
			return nil, err
		}
	}
	proj, err := LoadCompose(ctx, text, MergeEnv(shared, cfg.GetEnv()), dataDir, composeProject)
	if err != nil {
		return nil, Invalid("compose 无法解析：%v", err)
	}
	for name := range ServiceEnv(cfg, container) {
		if _, ok := proj.Services[name]; !ok {
			return nil, Invalid("环境变量里的容器 %q 不在这个容器组的 compose 中", name)
		}
	}
	if url := strings.TrimSpace(cfg.GetSource().GetGitUrl()); url != "" {
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") && !IsSSHURL(url) {
			return nil, Invalid("git 仓库地址只支持 SSH（git@host:repo.git、ssh://）和 HTTPS")
		}
		cfg.Source.GitUrl = url
		cfg.Source.GitRef = strings.TrimSpace(cfg.Source.GetGitRef())
	} else {
		cfg.Source = nil
	}
	cfg.Preserve = NormalizePreserve(cfg.GetPreserve())
	var deps []string
	seen := map[string]bool{}
	for _, d := range cfg.GetDependsOn() {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		if d == container || !NameRe.MatchString(d) {
			return nil, Invalid("依赖的容器 %q 不对", d)
		}
		seen[d] = true
		deps = append(deps, d)
	}
	cfg.DependsOn = deps
	return proj, nil
}

// Load 只解析容器的 compose（不做 Validate 里的其他检查），读取状态时用。
func Load(ctx context.Context, cfg *nagiv1.ContainerConfig, container string, shared []*nagiv1.EnvVar, dataDir, composeProject string) (*types.Project, error) {
	text, err := ComposeText(cfg, container)
	if err != nil {
		return nil, err
	}
	return LoadCompose(ctx, text, MergeEnv(shared, cfg.GetEnv()), dataDir, composeProject)
}

// MergeEnv 合并共享变量和容器变量，同名时以容器的为准（和 compose 按顺序读两个 --env-file 一样）。
func MergeEnv(shared, own []*nagiv1.EnvVar) []*nagiv1.EnvVar {
	out := append([]*nagiv1.EnvVar(nil), shared...)
	return append(out, own...)
}

// ReferencedVars 返回容器 compose 里引用的变量名。
func ReferencedVars(cfg *nagiv1.ContainerConfig, container string) map[string]bool {
	text, _ := ComposeText(cfg, container)
	out := map[string]bool{}
	for _, m := range varRefRe.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	return out
}

// SharedEnvChanged 判断容器引用的、而且没有被容器自己的变量覆盖的共享变量，在 before 和 after 里的值是否不同。
func SharedEnvChanged(cfg *nagiv1.ContainerConfig, container string, before, after []*nagiv1.EnvVar) bool {
	refs := ReferencedVars(cfg, container)
	for _, v := range cfg.GetEnv() {
		delete(refs, v.GetKey())
	}
	a, b := envMap(before), envMap(after)
	for k := range refs {
		va, oka := a[k]
		vb, okb := b[k]
		if oka != okb || va != vb {
			return true
		}
	}
	return false
}

func envMap(vars []*nagiv1.EnvVar) map[string]string {
	out := map[string]string{}
	for _, v := range vars {
		out[v.GetKey()] = v.GetValue()
	}
	return out
}

// CheckEnv 检查变量名合法、不重复。
func CheckEnv(vars []*nagiv1.EnvVar) error {
	seen := map[string]bool{}
	for _, v := range vars {
		if !envKeyRe.MatchString(v.GetKey()) {
			return Invalid("变量名 %q 不合法：只能包含字母、数字和下划线，且不能以数字开头", v.GetKey())
		}
		if seen[v.GetKey()] {
			return Invalid("变量 %s 重复", v.GetKey())
		}
		seen[v.GetKey()] = true
	}
	return nil
}

// LoadCompose 用 compose 官方的解析库在内存中解析 compose，不需要写文件。
func LoadCompose(ctx context.Context, text string, env []*nagiv1.EnvVar, dataDir, composeProject string) (*types.Project, error) {
	mapping := types.Mapping{}
	for _, v := range env {
		mapping[v.GetKey()] = v.GetValue()
	}
	return loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dataDir,
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yml", Content: []byte(text)}},
		Environment: mapping,
	}, func(o *loader.Options) {
		o.SetProjectName(composeProject, true)
		o.ResolvePaths = true
	})
}

// IsSSHURL 判断 git 地址是不是 SSH 地址。
func IsSSHURL(url string) bool {
	return strings.HasPrefix(url, "ssh://") || scpURLRe.MatchString(url)
}

// NormalizePreserve 把保留路径规范成相对 data/ 的 "a/b" 形式，去掉不合法的。
func NormalizePreserve(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range list {
		p = path.Clean(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/")))
		p = strings.TrimPrefix(strings.TrimPrefix(p, "./"), "/")
		if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// BindMountPaths 返回 compose 里挂载进容器的、位于 data/ 下的路径（相对 data/）。
// 这些路径自动保留，防止更新文件时把数据库目录之类的运行数据冲掉。
func BindMountPaths(proj *types.Project, dataDir string) []string {
	var out []string
	for _, svc := range proj.Services {
		for _, v := range svc.Volumes {
			if v.Type != types.VolumeTypeBind {
				continue
			}
			rel, err := filepath.Rel(dataDir, v.Source)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
				continue
			}
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out
}

// DataServices 返回挂载了 data/ 里文件的服务。文件更新后要重建这些服务的容器，
// 因为 compose 在配置没变时不会重建容器。
func DataServices(proj *types.Project, dataDir string) []string {
	var out []string
	for _, name := range proj.ServiceNames() {
		for _, v := range proj.Services[name].Volumes {
			if v.Type != types.VolumeTypeBind {
				continue
			}
			rel, err := filepath.Rel(dataDir, v.Source)
			if err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// PreservePaths 返回完整的保留路径：用户设置的 + compose 里挂载进容器的 data/ 下的目录。
func PreservePaths(cfg *nagiv1.ContainerConfig, proj *types.Project, dataDir string) []string {
	return NormalizePreserve(append(append([]string(nil), cfg.GetPreserve()...), BindMountPaths(proj, dataDir)...))
}

// IsStateful 判断容器是否有状态：用户指定的为准；自动判断时，用了命名 volume 或有保留路径就算有状态。
func IsStateful(cfg *nagiv1.ContainerConfig, proj *types.Project, preserve []string) bool {
	switch cfg.GetStateful() {
	case nagiv1.Stateful_STATEFUL_YES:
		return true
	case nagiv1.Stateful_STATEFUL_NO:
		return false
	}
	if len(preserve) > 0 {
		return true
	}
	for _, v := range proj.Volumes {
		if !v.External {
			return true
		}
	}
	return false
}

// ManagedVolume 判断命名 volume 是否由 Nagi 绑定到容器的 volumes/（在快照范围内）。
// external 的和用了自定义 driver 或 driver_opts 的保持原样，不在快照范围内。
func ManagedVolume(v types.VolumeConfig) bool {
	return !bool(v.External) && (v.Driver == "" || v.Driver == "local") && len(v.DriverOpts) == 0
}

// UnmanagedVolumes 返回不在快照范围内的命名 volume，按名字排序。
func UnmanagedVolumes(proj *types.Project) []string {
	var out []string
	for key, v := range proj.Volumes {
		if !ManagedVolume(v) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// HostNetworkServices 返回用 network_mode: host 的服务。
func HostNetworkServices(proj *types.Project) []string {
	var out []string
	for _, name := range proj.ServiceNames() {
		if proj.Services[name].NetworkMode == "host" {
			out = append(out, name)
		}
	}
	return out
}

// ---- 内部端口 ----

var internalPortRe = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)?):([0-9]{1,5})$`)

// CleanInternalPorts 检查并整理项目设置里的内部端口（"容器:端口"或"服务.容器组:端口"）。
func CleanInternalPorts(in []string) ([]string, error) {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		m := internalPortRe.FindStringSubmatch(s)
		if m == nil {
			return nil, Invalid("内部端口 %q 的格式不对，应该是\"容器:端口\"或\"服务.容器组:端口\"", s)
		}
		if p, _ := strconv.Atoi(m[3]); p < 1 || p > 65535 {
			return nil, Invalid("内部端口 %q 的端口号不对", s)
		}
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// InternalPortMap 把内部端口按"容器"或"服务.容器组"分组。
func InternalPortMap(list []string) map[string][]uint32 {
	out := map[string][]uint32{}
	for _, s := range list {
		if m := internalPortRe.FindStringSubmatch(s); m != nil {
			p, _ := strconv.Atoi(m[3])
			out[m[1]] = append(out[m[1]], uint32(p))
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
