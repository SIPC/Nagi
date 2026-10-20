package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
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
	for name, svc := range in.proj.Services {
		labels := map[string]string{
			LabelProject:   in.projectID,
			LabelContainer: in.container,
			LabelVersion:   strconv.FormatInt(in.version, 10),
		}
		if in.single {
			labels[LabelSingle] = "1"
		}
		s := map[string]any{"labels": labels}
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
			return nil, invalid("volume 名不能是 %q", key)
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
