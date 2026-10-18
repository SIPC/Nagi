package project

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// 管理标签：agent 靠它识别自己管理的容器。
const (
	LabelProject = "nagi.project"
	LabelMember  = "nagi.member"
	LabelVersion = "nagi.version"
	// LabelSingle 标出单容器成员的容器：它由 compose 加入项目内网，别名就是服务名（成员名）。
	LabelSingle = "nagi.single"
)

// overrideInput 是生成一个成员的叠加配置要用的东西。
type overrideInput struct {
	proj      *types.Project
	mp        memberPaths
	projectID string
	member    string
	version   int64
	single    bool
	// intranet：项目内网开启。单容器成员这时只在项目内网里，不建自己的网络。
	intranet bool
	// subnet 是成员自己的默认网络的网段（compose 成员，或者内网关闭时的单容器成员）。
	subnet         string
	senv           map[string][]*nagiv1.EnvVar
	pins           map[string]string
	managedRestart bool
}

// buildOverride 生成 Nagi 的叠加配置。override 里写的值会覆盖用户的值，
// 所以默认值只补在用户没写的地方。
// managedRestart 时容器的重启策略一律写成 no，由节点代管（见 restart.go）。
// compose 成员不在这里加入项目内网和共享网络：compose 会把服务原名注册到它加入的每个网络上，
// 由节点用 network connect 只带指定的别名接入（见 network.go）。
func buildOverride(in overrideInput) (map[string]any, error) {
	services := map[string]any{}
	joinIntranet := in.single && in.intranet
	for name, svc := range in.proj.Services {
		labels := map[string]string{
			LabelProject: in.projectID,
			LabelMember:  in.member,
			LabelVersion: strconv.FormatInt(in.version, 10),
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
		// 单容器成员只在项目内网里：服务名就是成员名，compose 自动把它注册为别名。
		out["networks"] = map[string]any{
			"nagi": map[string]any{"name": projectNetwork(in.projectID), "external": true},
		}
	} else {
		// 成员自己的默认网络，用从节点容器网段池分配的网段。
		out["networks"] = map[string]any{
			"default": map[string]any{
				"ipam": map[string]any{
					"config": []map[string]string{{"subnet": in.subnet}},
				},
			},
		}
	}

	// 命名 volume 落到成员目录的 volumes/<名字>。外部 volume 和自定义 driver 的保持原样。
	volumes := map[string]any{}
	for key, v := range in.proj.Volumes {
		if !managedVolume(v) {
			continue
		}
		dir := filepath.Join(in.mp.volumesDir(), key)
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

// managedVolume 判断命名 volume 是否由 Nagi 绑定到成员的 volumes/（在快照范围内）。
// external 的和用了自定义 driver 或 driver_opts 的保持原样，不在快照范围内。
func managedVolume(v types.VolumeConfig) bool {
	return !bool(v.External) && (v.Driver == "" || v.Driver == "local") && len(v.DriverOpts) == 0
}

// unmanagedVolumes 返回不在快照范围内的命名 volume，按名字排序。
func unmanagedVolumes(proj *types.Project) []string {
	var out []string
	for key, v := range proj.Volumes {
		if !managedVolume(v) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// hostNetworkServices 返回用 network_mode: host 的服务。
func hostNetworkServices(proj *types.Project) []string {
	var out []string
	for _, name := range proj.ServiceNames() {
		if proj.Services[name].NetworkMode == "host" {
			out = append(out, name)
		}
	}
	return out
}
