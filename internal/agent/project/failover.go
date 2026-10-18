package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
)

// 顶替（见 docs/failover.md）：项目这一层只提供启停和交接用的操作，租约由节点管理。顶替按整个项目，所有容器一起。

// Fence 在租约到期前停止项目的所有容器：先正常停止，到 deadline 还没停完就强制结束容器。
// 只停止不删除，重新拿到租约后 Unfence 原样启动。
func (m *Manager) Fence(ctx context.Context, id string, deadline time.Time, onLine func(string)) error {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return err
	}
	p := paths{root: row.Dir}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return m.docker.Kill(context.WithoutCancel(ctx), projectLabel(id))
	}
	containers, err := containerNames(ctx, pdb)
	pdb.Close()
	if err != nil {
		return m.docker.Kill(context.WithoutCancel(ctx), projectLabel(id))
	}
	sctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for _, name := range containers {
		if _, err := os.Stat(p.container(name).overrideFile()); err != nil {
			continue // 从未部署过
		}
		if err := compose.Stop(sctx, m.composeFiles(id, p, name), onLine); err != nil {
			onLine("来不及正常停止，强制结束容器")
			return m.docker.Kill(context.WithoutCancel(ctx), projectLabel(id))
		}
	}
	return nil
}

// Unfence 重新拿到租约后启动项目（容器还在时就是启动，不在时按当前文件创建）。
func (m *Manager) Unfence(ctx context.Context, id string, onLine func(string)) error {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return err
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return err
	}
	defer pdb.Close()
	return m.upAll(ctx, pdb, paths{root: row.Dir}, id, onLine)
}

// Stateful 判断项目是否有状态：只要有一个容器有状态就算（规则见 isStateful）。
func (m *Manager) Stateful(ctx context.Context, id string) bool {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return false
	}
	pdb, err := openProject(ctx, row.Dir)
	if err != nil {
		return false
	}
	defer pdb.Close()
	return m.projectStateful(ctx, pdb, id, paths{root: row.Dir})
}

// Has 判断本节点上有没有这个项目。
func (m *Manager) Has(ctx context.Context, id string) bool {
	_, err := m.projectRow(ctx, id)
	return err == nil
}

// Running 判断项目是否有正在运行的容器。
func (m *Manager) Running(ctx context.Context, id string) bool {
	ok, err := m.docker.Running(ctx, projectLabel(id))
	return err == nil && ok
}

// Demote 项目已经被别的节点顶替：停止并删除所有容器的容器，把项目移出本节点，
// 项目目录改名保留（<目录>.replaced-<时间>），原目录空出来，以后本节点顶替回来时恢复到这里。
func (m *Manager) Demote(ctx context.Context, id string, onLine func(string)) (string, error) {
	l := m.lock(id)
	l.Lock()
	defer l.Unlock()
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return "", err
	}
	p := paths{root: row.Dir}
	if pdb, err := openProject(ctx, row.Dir); err == nil {
		containers, _ := containerNames(ctx, pdb)
		pdb.Close()
		for _, name := range containers {
			if err := m.down(ctx, id, p, name, onLine); err != nil {
				_ = m.docker.Kill(ctx, projectLabel(id))
			}
		}
	}
	_ = m.docker.Kill(ctx, projectLabel(id))
	_ = m.docker.RemoveNetwork(ctx, projectNetwork(id))
	if err := m.releaseProject(ctx, id); err != nil {
		return "", err
	}
	kept := fmt.Sprintf("%s.replaced-%s", row.Dir, time.Now().Format("20060102-150405"))
	if err := os.Rename(row.Dir, kept); err != nil {
		return row.Dir, err
	}
	return kept, nil
}

// ReplicaStateful 读取副本里记录的项目是否有状态。
func ReplicaStateful(root string) bool {
	info, err := readReplicaInfo(root)
	return err == nil && info.Stateful
}

// PrePull 热备：按副本里各容器的 compose 提前拉好镜像，顶替时只需要启动容器。失败不影响顶替（启动时会再拉）。
func PrePull(ctx context.Context, root string, onLine func(string)) error {
	if _, err := readReplicaInfo(root); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(root, "containers"))
	if err != nil {
		return nil // 没有容器
	}
	tmp := filepath.Join(root, "..", "prepull")
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	// 拉镜像不需要网段等部署设置，用一个空的叠加配置。
	empty := filepath.Join(tmp, "override.yml")
	if err := os.WriteFile(empty, []byte("services: {}\n"), 0o600); err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		mp := containerPaths{root: filepath.Join(root, "containers", e.Name())}
		if _, err := os.Stat(mp.composeFile()); err != nil {
			continue
		}
		files := compose.Files{
			ProjectName: "prepull-" + e.Name(),
			ProjectDir:  tmp,
			ComposeFile: mp.composeFile(),
			Override:    empty,
		}
		for _, f := range []string{mp.sharedEnvFile(), mp.envFile()} {
			if _, err := os.Stat(f); err == nil {
				files.EnvFiles = append(files.EnvFiles, f)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "config", "docker", "config.json")); err == nil {
			files.DockerConfig = filepath.Join(root, "config", "docker")
		}
		if err := compose.Pull(ctx, files, onLine); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("容器 %s：%w", e.Name(), err)
		}
	}
	return firstErr
}
