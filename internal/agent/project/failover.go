package project

import (
	"context"
	"os"
	"time"

	"github.com/SIPC/nagi/internal/agent/compose"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/protobuf/proto"
)

// 顶替（见 docs/failover.md）：这一层只提供启停和交接用的操作，租约由节点管理。顶替按容器。

// Has 判断本节点上有没有这个容器。
func (m *Manager) Has(ctx context.Context, projectID, container string) bool {
	_, err := m.Get(ctx, projectID, container)
	return err == nil
}

// Fence 在租约到期前停止容器：先正常停止，到 deadline 还没停完就强制结束。只停止不删除，重新拿到租约后 Unfence 原样启动。
func (m *Manager) Fence(ctx context.Context, projectID, container string, deadline time.Time, onLine func(string)) error {
	label := containerLabel(projectID, container)
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return m.docker.Kill(context.WithoutCancel(ctx), label)
	}
	if _, err := os.Stat(containerPaths{root: c.Spec.GetDir()}.overrideFile()); err != nil {
		return nil // 从未部署过
	}
	sctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := compose.Stop(sctx, composeFiles(c.Spec), onLine); err != nil {
		onLine("来不及正常停止，强制结束容器")
		return m.docker.Kill(context.WithoutCancel(ctx), label)
	}
	return nil
}

// Unfence 重新拿到租约后启动容器。
func (m *Manager) Unfence(ctx context.Context, projectID, container string, onLine func(string)) error {
	l := m.lock(projectID, container)
	l.Lock()
	defer l.Unlock()
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return err
	}
	if c.Spec.GetVersion() == 0 {
		return nil
	}
	if err := compose.Up(ctx, composeFiles(c.Spec), onLine); err != nil {
		return err
	}
	return m.SyncNetworks(ctx, projectID)
}

// PrePull 热备：按副本的配置提前拉好镜像，顶替时只需要启动容器。tmp 是临时目录。失败不影响顶替（启动时会再拉）。
func PrePull(ctx context.Context, s *nagiv1.ContainerSpec, tmp string, onLine func(string)) error {
	if s.GetVersion() == 0 {
		return nil
	}
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	mp := containerPaths{root: tmp}
	if err := os.MkdirAll(mp.dataDir(), 0o700); err != nil {
		return err
	}
	if err := writeSecrets(mp, s); err != nil {
		return err
	}
	if err := writeConfigFiles(mp, s.GetConfig(), s.GetName(), s.GetSharedEnv()); err != nil {
		return err
	}
	// 拉镜像不需要网段等部署设置：只把镜像固定到部署时的摘要。
	ov := "services:\n"
	for svc, img := range s.GetImages() {
		ov += "  " + svc + ":\n    image: " + img + "\n"
	}
	if len(s.GetImages()) == 0 {
		ov = "services: {}\n"
	}
	if err := os.WriteFile(mp.overrideFile(), []byte(ov), 0o600); err != nil {
		return err
	}
	sp := proto.Clone(s).(*nagiv1.ContainerSpec)
	sp.Dir = tmp
	files := composeFiles(sp)
	files.ProjectName = "prepull-" + s.GetProjectId() + "-" + s.GetName()
	return compose.Pull(ctx, files, onLine)
}
