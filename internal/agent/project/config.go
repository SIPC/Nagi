package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// formService 是镜像表单生成的 compose 里唯一的服务名。
const formService = "app"

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// composeText 返回配置对应的 compose 原文：有镜像表单时由表单生成。
func composeText(cfg *nagiv1.ProjectConfig) (string, error) {
	if cfg.GetForm() != nil {
		return formCompose(cfg.GetForm())
	}
	return cfg.GetCompose(), nil
}

// serviceEnv 返回每个服务的环境变量。镜像表单的环境变量属于唯一的服务。
func serviceEnv(cfg *nagiv1.ProjectConfig) map[string][]*nagiv1.EnvVar {
	out := map[string][]*nagiv1.EnvVar{}
	for name, list := range cfg.GetServiceEnv() {
		if len(list.GetVars()) > 0 {
			out[name] = list.GetVars()
		}
	}
	if f := cfg.GetForm(); f != nil && len(f.GetEnv()) > 0 {
		out[formService] = append(out[formService], f.GetEnv()...)
	}
	return out
}

// formCompose 把镜像表单转换成 compose。环境变量不写进 compose，而是放在服务级环境变量文件里，
// 这样导出的 compose 里不会带着密钥。
func formCompose(f *nagiv1.ImageForm) (string, error) {
	image := strings.TrimSpace(f.GetImage())
	if image == "" {
		return "", invalid("镜像不能为空")
	}
	svc := map[string]any{"image": image}
	if cmd := strings.TrimSpace(f.GetCommand()); cmd != "" {
		svc["command"] = cmd
	}
	var ports []string
	for _, p := range f.GetPorts() {
		if p.GetPublished() == 0 || p.GetTarget() == 0 || p.GetPublished() > 65535 || p.GetTarget() > 65535 {
			return "", invalid("端口必须在 1 到 65535 之间")
		}
		proto := strings.ToLower(p.GetProtocol())
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			return "", invalid("端口协议只能是 tcp 或 udp")
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
			return "", invalid("持久化目录必须是容器内的绝对路径：%s", target)
		}
		name := volumeName(target, used)
		volumes[name] = map[string]any{}
		mounts = append(mounts, name+":"+target)
	}
	if wd := strings.TrimSpace(f.GetWorkdir()); wd != "" {
		if !strings.HasPrefix(wd, "/") {
			return "", invalid("工作目录必须是容器内的绝对路径")
		}
		// compose 的相对路径以项目的 data/ 为准，所以 "./" 就是 data/。
		mounts = append(mounts, "./:"+wd)
		svc["working_dir"] = wd
	}
	if len(mounts) > 0 {
		svc["volumes"] = mounts
	}

	doc := map[string]any{"services": map[string]any{formService: svc}}
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

// validate 校验配置：compose 能解析、变量名合法、服务级变量对应的服务存在、开放设置有效。
// 会把 compose 里的 ports 补进 cfg.Exposures。
func validate(ctx context.Context, cfg *nagiv1.ProjectConfig, workDir, composeProject string) (*types.Project, error) {
	text, err := composeText(cfg)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, invalid("compose 不能为空")
	}
	if err := checkEnv(cfg.GetEnv()); err != nil {
		return nil, err
	}
	for _, vars := range serviceEnv(cfg) {
		if err := checkEnv(vars); err != nil {
			return nil, err
		}
	}
	proj, err := loadCompose(ctx, text, cfg.GetEnv(), workDir, composeProject)
	if err != nil {
		return nil, invalid("compose 无法解析：%v", err)
	}
	for name := range serviceEnv(cfg) {
		if _, ok := proj.Services[name]; !ok {
			return nil, invalid("服务级环境变量里的服务 %q 不在 compose 中", name)
		}
	}
	// compose 里的 ports 自动转成开放设置，写回配置里，界面上能看到并修改。
	cfg.Exposures = materializeExposures(proj, cfg)
	if err := checkExposures(proj, cfg.Exposures); err != nil {
		return nil, err
	}
	return proj, nil
}

func checkEnv(vars []*nagiv1.EnvVar) error {
	seen := map[string]bool{}
	for _, v := range vars {
		if !envKeyRe.MatchString(v.GetKey()) {
			return invalid("变量名 %q 不合法：只能包含字母、数字和下划线，且不能以数字开头", v.GetKey())
		}
		if seen[v.GetKey()] {
			return invalid("变量 %s 重复", v.GetKey())
		}
		seen[v.GetKey()] = true
	}
	return nil
}

// loadCompose 用 compose 官方的解析库在内存中解析 compose，不需要写文件。
func loadCompose(ctx context.Context, text string, env []*nagiv1.EnvVar, workDir, composeProject string) (*types.Project, error) {
	mapping := types.Mapping{}
	for _, v := range env {
		mapping[v.GetKey()] = v.GetValue()
	}
	return loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  workDir,
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yml", Content: []byte(text)}},
		Environment: mapping,
	}, func(o *loader.Options) {
		o.SetProjectName(composeProject, true)
		o.ResolvePaths = true
	})
}

// writeConfigFiles 把一个版本的配置写成 compose 需要的文件：compose.yml、.env、env/<服务>.env。
func writeConfigFiles(p paths, cfg *nagiv1.ProjectConfig) error {
	text, err := composeText(cfg)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p.composeFile(), []byte(text), 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(p.envFile(), envFile(cfg.GetEnv()), 0o600); err != nil {
		return err
	}
	// 重新生成服务级环境变量文件，删掉不再需要的。
	if err := os.RemoveAll(p.serviceEnvDir()); err != nil {
		return err
	}
	senv := serviceEnv(cfg)
	if len(senv) == 0 {
		return nil
	}
	if err := os.MkdirAll(p.serviceEnvDir(), 0o700); err != nil {
		return err
	}
	for name, vars := range senv {
		if err := writeFileAtomic(p.serviceEnvFile(name), envFile(vars), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// envFile 生成 compose 能读的 .env 内容。值统一用单引号包起来，原样保留，不做变量替换；
// 值里本身有单引号时改用双引号并转义。
func envFile(vars []*nagiv1.EnvVar) []byte {
	sorted := append([]*nagiv1.EnvVar(nil), vars...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].GetKey() < sorted[j].GetKey() })
	var b strings.Builder
	for _, v := range sorted {
		b.WriteString(v.GetKey())
		b.WriteByte('=')
		b.WriteString(quoteEnv(v.GetValue()))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func quoteEnv(v string) string {
	if !strings.ContainsAny(v, "'\n\r") {
		return "'" + v + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "$", "$$")
	return `"` + r.Replace(v) + `"`
}

func (p paths) serviceEnvDir() string { return filepath.Join(p.configDir(), "env") }
func (p paths) serviceEnvFile(service string) string {
	return filepath.Join(p.serviceEnvDir(), service+".env")
}
