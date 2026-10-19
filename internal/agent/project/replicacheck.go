package project

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SIPC/nagi/internal/agent/store/projectstore"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/types"
	"google.golang.org/protobuf/proto"
)

// 从副本启动项目（恢复副本、顶替、迁移过来）或者按配置备份重建时，配置来自别的节点。那个节点被攻破的话，
// 它可以在副本里放一份特权 compose（例如挂载宿主机的 /），等本节点启动时拿到本节点的 root。
// 所以这些情况下，配置里有能碰到宿主机的设置时不启动，除非管理员在面板上允许了这个项目
// （由面板直接下发，见 Manager.TrustedProject）。普通容器只能碰到自己的项目目录。

// dangerousSettings 返回 compose 里能碰到宿主机或者别的项目的设置。base 是容器目录，挂载它下面的路径是正常的。
func dangerousSettings(cp *types.Project, base string) []string {
	var out []string
	add := func(svc, what string) { out = append(out, svc+"："+what) }
	for _, name := range cp.ServiceNames() {
		svc := cp.Services[name]
		if svc.Privileged {
			add(name, "特权模式（privileged）")
		}
		for _, m := range []struct{ field, value string }{
			{"network_mode", svc.NetworkMode}, {"pid", svc.Pid}, {"ipc", svc.Ipc}, {"uts", svc.Uts}, {"userns_mode", svc.UserNSMode}, {"cgroup", svc.Cgroup},
		} {
			if m.value == "host" || strings.HasPrefix(m.value, "container:") {
				add(name, m.field+": "+m.value)
			}
		}
		if len(svc.CapAdd) > 0 {
			add(name, "额外的内核权限 cap_add: "+strings.Join(svc.CapAdd, ","))
		}
		if len(svc.Devices) > 0 {
			add(name, "设备 devices")
		}
		for _, o := range svc.SecurityOpt {
			if strings.Contains(o, "unconfined") || strings.Contains(o, "disable") {
				add(name, "security_opt: "+o)
			}
		}
		for _, v := range svc.VolumesFrom {
			if strings.HasPrefix(v, "container:") {
				add(name, "volumes_from: "+v)
			}
		}
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind {
				rel, err := filepath.Rel(base, v.Source)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
					add(name, "挂载宿主机路径 "+v.Source)
				}
			}
		}
	}
	var vols []string
	for key, v := range cp.Volumes {
		if !managedVolume(v) {
			vols = append(vols, key)
		}
	}
	sort.Strings(vols)
	for _, key := range vols {
		out = append(out, "命名 volume "+key+" 是 external 或者用了自定义 driver")
	}
	return out
}

// replicaRefusal 检查要从别的节点来的配置启动的容器，有危险设置、而项目没有被面板允许时返回拒绝的说明。
// configs 的键是容器名。
func (m *Manager) replicaRefusal(ctx context.Context, id, trustID string, p paths, configs map[string]*nagiv1.ContainerConfig, shared map[string][]*nagiv1.EnvVar) error {
	if m.TrustedProject != nil && m.TrustedProject(trustID) {
		return nil
	}
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	var reasons []string
	for _, name := range names {
		mp := p.container(name)
		cp, err := loadContainer(ctx, proto.Clone(configs[name]).(*nagiv1.ContainerConfig), name, shared[name], mp.dataDir(), composeProjectName(id, name))
		if err != nil {
			continue // 配置本身有问题的，启动时会报错
		}
		for _, r := range dangerousSettings(cp, mp.root) {
			reasons = append(reasons, "容器 "+name+" 的 "+r)
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	return invalid("配置来自别的节点，里面有能碰到本节点宿主机的设置，为了安全没有启动：%s。"+
		"这样做是防止那个节点被攻破后借副本控制本节点。确认这个项目的配置可信的话，在面板的项目设置里勾选"+
		"「允许在备份节点上使用特权配置」，再重新操作", strings.Join(reasons, "；"))
}

// deployedConfigs 返回各容器部署的版本的配置和当时的共享变量。
func deployedConfigs(ctx context.Context, pdb *projectDB, containers []projectstore.Container) (map[string]*nagiv1.ContainerConfig, map[string][]*nagiv1.EnvVar, error) {
	configs := map[string]*nagiv1.ContainerConfig{}
	shared := map[string][]*nagiv1.EnvVar{}
	for _, mem := range containers {
		v, err := pdb.deployed(ctx, mem)
		if err != nil {
			return nil, nil, err
		}
		if v == nil {
			continue
		}
		cfg, err := unmarshalConfig(v.Config)
		if err != nil {
			return nil, nil, fmt.Errorf("容器 %s 的配置: %w", mem.Name, err)
		}
		configs[mem.Name], shared[mem.Name] = cfg, unmarshalEnv(v.SharedEnv)
	}
	return configs, shared, nil
}
