package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// 管理标签：agent 靠它识别自己管理的容器。
const (
	LabelProject = "nagi.project"
	LabelVersion = "nagi.version"
)

// buildOverride 生成 Nagi 的叠加配置。override 里写的值会覆盖用户的值，
// 所以默认值只补在用户没写的地方。
func buildOverride(proj *types.Project, p paths, projectID string, version int64, subnet string, senv map[string][]*nagiv1.EnvVar) (map[string]any, error) {
	services := map[string]any{}
	for name, svc := range proj.Services {
		s := map[string]any{
			"labels": map[string]string{
				LabelProject: projectID,
				LabelVersion: strconv.FormatInt(version, 10),
			},
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
		if len(senv[name]) > 0 {
			s["env_file"] = []string{filepath.ToSlash(p.serviceEnvFile(name))}
		}
		services[name] = s
	}

	out := map[string]any{"services": services}

	// 项目默认网络使用从节点容器网段池分配的网段。
	out["networks"] = map[string]any{
		"default": map[string]any{
			"ipam": map[string]any{
				"config": []map[string]string{{"subnet": subnet}},
			},
		},
	}

	// 命名 volume 落到项目目录的 volumes/<名字>。外部 volume 和自定义 driver 的保持原样。
	volumes := map[string]any{}
	for key, v := range proj.Volumes {
		if v.External || (v.Driver != "" && v.Driver != "local") || len(v.DriverOpts) > 0 {
			continue
		}
		dir := filepath.Join(p.volumesDir(), key)
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
