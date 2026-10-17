package project

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 顶替（见 docs/failover.md）：项目这一层只提供启停和交接用的操作，租约由节点管理。

// Fence 在租约到期前停止项目：先正常停止，到 deadline 还没停完就强制结束容器。
// 只停止不删除，重新拿到租约后 Unfence 原样启动。
func (m *Manager) Fence(ctx context.Context, id string, deadline time.Time, onLine func(string)) error {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return err
	}
	p := paths{root: row.Dir}
	if _, err := os.Stat(p.overrideFile()); err != nil {
		return nil // 从未部署过
	}
	sctx, cancel := context.WithDeadline(ctx, deadline)
	err = compose.Stop(sctx, composeFiles(id, p), onLine)
	cancel()
	if err == nil {
		return nil
	}
	onLine("来不及正常停止，强制结束容器")
	return m.docker.KillProject(context.WithoutCancel(ctx), composeProjectName(id))
}

// Unfence 重新拿到租约后启动项目（容器还在时就是启动，不在时按当前文件创建）。
func (m *Manager) Unfence(ctx context.Context, id string, onLine func(string)) error {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return err
	}
	p := paths{root: row.Dir}
	if _, err := os.Stat(p.overrideFile()); err != nil {
		return nil
	}
	return compose.Up(ctx, composeFiles(id, p), onLine)
}

// Stateful 判断项目是否有状态（规则见 isStateful）。
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
	cfg, _, err := pdb.latest(ctx)
	if err != nil {
		return false
	}
	p := paths{root: row.Dir}
	proj, err := validate(ctx, proto.Clone(cfg).(*nagiv1.ProjectConfig), p.dataDir(), composeProjectName(id))
	if err != nil {
		return false
	}
	return isStateful(cfg, proj, preservePaths(cfg, proj, p.dataDir()))
}

// Has 判断本节点上有没有这个项目。
func (m *Manager) Has(ctx context.Context, id string) bool {
	_, err := m.projectRow(ctx, id)
	return err == nil
}

// Running 判断项目是否有正在运行的容器。
func (m *Manager) Running(ctx context.Context, id string) bool {
	ok, err := m.docker.ProjectRunning(ctx, composeProjectName(id))
	return err == nil && ok
}

// Demote 项目已经被别的节点顶替：停止并删除容器，把项目移出本节点，
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
	if _, err := os.Stat(p.overrideFile()); err == nil {
		if err := compose.Down(ctx, composeFiles(id, p), true, onLine); err != nil {
			_ = m.docker.KillProject(ctx, composeProjectName(id))
		}
	}
	if err := m.nodeQ.DeleteProject(ctx, id); err != nil {
		return "", err
	}
	os.RemoveAll(filepath.Join(m.stateDir, id))
	kept := fmt.Sprintf("%s.replaced-%s", row.Dir, time.Now().Format("20060102-150405"))
	if err := os.Rename(row.Dir, kept); err != nil {
		return row.Dir, err
	}
	return kept, nil
}

// ReplicaStateful 读取副本里记录的项目是否有状态。
func ReplicaStateful(root string) bool {
	raw, err := os.ReadFile(filepath.Join(root, "project.json"))
	if err != nil {
		return false
	}
	var info replicaInfo
	return json.Unmarshal(raw, &info) == nil && info.Stateful
}

// PrePull 热备：按副本里的配置提前拉好镜像，顶替时只需要启动容器。失败不影响顶替（启动时会再拉）。
func PrePull(ctx context.Context, root string, onLine func(string)) error {
	raw, err := os.ReadFile(filepath.Join(root, "project.json"))
	if err != nil {
		return err
	}
	var info replicaInfo
	cfg := &nagiv1.ProjectConfig{}
	if err := json.Unmarshal(raw, &info); err != nil {
		return err
	}
	if err := protojson.Unmarshal(info.Config, cfg); err != nil {
		return err
	}
	prep := paths{root: filepath.Join(root, "..", "prepull")}
	os.RemoveAll(prep.root)
	defer os.RemoveAll(prep.root)
	for _, d := range []string{prep.configDir(), prep.dataDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := writeConfigFiles(prep, cfg); err != nil {
		return err
	}
	// 拉镜像不需要网段等部署设置，用一个空的叠加配置。
	if err := os.WriteFile(prep.overrideFile(), []byte("services: {}\n"), 0o600); err != nil {
		return err
	}
	return compose.Pull(ctx, composeFiles("prepull", prep), onLine)
}
