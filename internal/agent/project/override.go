package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SIPC/nagi/internal/agent/fileops"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// 管理标签：agent 靠它识别自己管理的容器。
const (
	LabelProject   = "nagi.project"
	LabelContainer = "nagi.container"
	LabelVersion   = "nagi.version"
	// LabelSingle 标出单个容器的容器：它由 compose 加入项目内网，别名就是服务名（容器名）。
	LabelSingle = "nagi.single"
)

// overrideInput 是生成一个容器的叠加配置要用的东西。
type overrideInput struct {
	proj      *types.Project
	mp        containerPaths
	projectID string
	container string
	version   int64
	single    bool
	// subnet 是容器组自己的默认网络的网段。单个容器只在项目内网里，不建自己的网络。
	subnet         string
	senv           map[string][]*nagiv1.EnvVar
	pins           map[string]string
	managedRestart bool
	// projectDir 是现有目录（见 docs/agent.md#现有目录），为空时不是。
	projectDir string
}

// replaceDotEnv 把 env_file 里的 dotEnv 换成 ours，返回新列表；没有引用 dotEnv 时 ok 为 false。
func replaceDotEnv(files []types.EnvFile, dotEnv, ours string) (out []string, ok bool) {
	for _, f := range files {
		if filepath.Clean(f.Path) == dotEnv {
			out, ok = append(out, ours), true
			continue
		}
		out = append(out, f.Path)
	}
	return out, ok
}

// buildOverride 生成 Nagi 的叠加配置。override 里写的值会覆盖用户的值，
// 所以默认值只补在用户没写的地方。
// managedRestart 时容器的重启策略一律写成 no，由节点代管（见 restart.go）。
// 容器组不在这里加入项目内网：compose 会把服务原名注册到它加入的每个网络上，
// 由节点用 network connect 只带指定的别名接入（见 network.go）。
func buildOverride(in overrideInput) (map[string]any, error) {
	if err := checkMountSources(in.proj, in.mp.root); err != nil {
		return nil, err
	}
	services := map[string]any{}
	joinIntranet := in.single
	// CLI 还可能通过 COMPOSE_PROFILES 或显式服务名启用 profile 服务。
	for name, svc := range in.proj.AllServices() {
		labels := map[string]string{
			LabelProject:   in.projectID,
			LabelContainer: in.container,
			LabelVersion:   strconv.FormatInt(in.version, 10),
		}
		if len(svc.LabelFiles) > 0 {
			fileLabels := map[string]string{}
			remaining := int64(64 << 10)
			expansionBudget := 64 << 10
			for _, path := range svc.LabelFiles {
				base := in.mp.root
				if in.projectDir != "" {
					base = in.projectDir
				}
				rel, err := filepath.Rel(base, path)
				if err != nil {
					return nil, invalid("无法读取标签文件")
				}
				// 显式引用容器目录以外的文件已由特权配置权限控制；目录以内的路径必须经约束根打开。
				if !filepath.IsLocal(rel) {
					base, rel = filepath.Dir(path), filepath.Base(path)
				}
				root, err := os.OpenRoot(base)
				if err != nil {
					return nil, invalid("无法读取标签文件")
				}
				info, err := root.Lstat(rel)
				if err != nil || !info.Mode().IsRegular() || info.Size() > remaining {
					root.Close()
					return nil, invalid("标签文件必须是普通文件，且每个服务合计不能超过 64 KiB")
				}
				f, ok, err := fileops.OpenRegular(root, rel, info)
				root.Close()
				if err != nil || !ok {
					return nil, invalid("无法安全读取标签文件")
				}
				data, err := io.ReadAll(io.LimitReader(f, remaining+1))
				f.Close()
				if err != nil || int64(len(data)) > remaining {
					return nil, invalid("标签文件读取失败或超过 64 KiB")
				}
				remaining -= int64(len(data))
				// 原文和所有被替换的值共享预算，在库拼接字符串之前扣除。
				// 必须拦截同一文件内的引用，否则短小的倍增链仍能耗尽内存。
				expansionBudget -= len(data)
				exceeded := expansionBudget < 0
				values := map[string]string{}
				err = dotenv.ParseWithFormat(strings.NewReader(string(data)), "", values, func(key string) (string, bool) {
					value, ok := fileLabels[key]
					if !ok {
						value, ok = values[key]
					}
					if exceeded || len(value) > expansionBudget {
						exceeded = true
						return "", true // 不允许库回退到内部 map 中未经预算检查的值。
					}
					expansionBudget -= len(value)
					return value, ok
				}, dotenv.DotEnv)
				if exceeded {
					return nil, invalid("标签文件展开预算不能超过 64 KiB")
				}
				if err != nil {
					return nil, invalid("标签文件格式不正确")
				}
				for k, v := range values {
					fileLabels[k] = v
				}
			}
			for k, v := range fileLabels {
				labels[k] = strings.ReplaceAll(v, "$", "$$")
			}
			for k, v := range svc.Labels {
				labels[k] = strings.ReplaceAll(v, "$", "$$")
			}
			labels[LabelProject], labels[LabelContainer], labels[LabelVersion] = in.projectID, in.container, strconv.FormatInt(in.version, 10)
		}
		if in.single {
			labels[LabelSingle] = "1"
		}
		s := map[string]any{"labels": labels}
		if len(svc.LabelFiles) > 0 {
			// compose 不再打开可被容器替换的原文件，只使用上面安全读取的快照。
			s["label_file"] = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!reset", Style: yaml.FlowStyle}
		}
		if svc.Logging == nil {
			s["logging"] = map[string]any{
				"driver":  "json-file",
				"options": map[string]string{"max-size": "10m", "max-file": "3"},
			}
		}
		hasRestartPolicy := svc.Deploy != nil && svc.Deploy.RestartPolicy != nil
		if svc.Restart == "" && !hasRestartPolicy {
			s["restart"] = "unless-stopped"
		}
		if in.managedRestart {
			s["restart"] = "no"
			if hasRestartPolicy {
				s["deploy"] = map[string]any{"restart_policy": map[string]any{"condition": "none"}}
			}
		}
		// 对外访问统一由 Caddy 负责，去掉容器自己的端口映射（需要 compose 2.24+ 的 !reset）。
		if len(svc.Ports) > 0 && svc.NetworkMode != "host" {
			s["ports"] = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!reset", Style: yaml.FlowStyle}
		}
		// 回滚时固定使用当时的镜像（带摘要），即使标签是 latest 也能回到那一份镜像。
		if img := in.pins[name]; img != "" {
			s["image"] = img
		}
		if len(in.senv[name]) > 0 {
			s["env_file"] = []string{filepath.ToSlash(in.mp.serviceEnvFile(name))}
		}
		// 现有目录：compose 里的 env_file: .env 改读面板的变量（导入时从这份 .env 复制过来，之后以面板为准），
		// 原来的 .env 不再使用。要整个替换列表，所以用 !override。
		if in.projectDir != "" {
			if files, ok := replaceDotEnv(svc.EnvFiles, filepath.Join(in.projectDir, ".env"), in.mp.envFile()); ok {
				if len(in.senv[name]) > 0 {
					files = append(files, in.mp.serviceEnvFile(name))
				}
				seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!override"}
				for _, f := range files {
					seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: filepath.ToSlash(f)})
				}
				s["env_file"] = seq
			}
		}
		if joinIntranet && svc.NetworkMode == "" {
			s["networks"] = []string{"nagi"}
		}
		services[name] = s
	}

	out := map[string]any{"services": services}

	if joinIntranet {
		// 单个容器只在项目内网里：服务名就是容器名，compose 自动把它注册为别名。
		out["networks"] = map[string]any{
			"nagi": map[string]any{"name": projectNetwork(in.projectID), "external": true},
		}
	} else {
		// 容器自己的默认网络，用从节点容器网段池分配的网段。
		out["networks"] = map[string]any{
			"default": map[string]any{
				"ipam": map[string]any{
					"config": []map[string]string{{"subnet": in.subnet}},
				},
			},
		}
	}

	// 命名 volume 落到容器目录的 volumes/<名字>。外部 volume 和自定义 driver 的保持原样。
	volumes := map[string]any{}
	for key, v := range in.proj.Volumes {
		if !spec.ManagedVolume(v) {
			continue
		}
		if key == "." || key == ".." {
			return nil, invalid("卷名不能是 %s", key)
		}
		dir := filepath.Join(in.mp.volumesDir(), key)
		if err := noSymlinkBelow(in.mp.root, dir); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建 volume 目录: %w", err)
		}
		volumes[key] = map[string]any{
			"driver": "local",
			"driver_opts": map[string]string{
				"type":   "none",
				"o":      "bind",
				"device": filepath.ToSlash(dir),
			},
		}
	}
	if len(volumes) > 0 {
		out["volumes"] = volumes
	}
	return out, nil
}

func writeOverride(path string, override map[string]any) error {
	data, err := yaml.Marshal(override)
	if err != nil {
		return err
	}
	header := []byte("# 由 Nagi 生成，每次部署时重新生成，请勿手动修改。\n")
	return writeFileAtomic(path, append(header, data...), 0o644)
}
