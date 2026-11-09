package project

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SIPC/nagi/internal/agent/compose"
	"github.com/SIPC/nagi/internal/agent/docker"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 现有目录（见 docs/agent.md#现有目录）：容器组的 compose 直接在节点上一个现有的目录里运行，数据留在原处。
// 第一次部署时，目录里原来用 docker compose 起的容器由面板接管。

// replacedSuffix 加在被接管的容器名后面：原来的容器名（container_name）会和新容器冲突。
const replacedSuffix = "-replaced-by-nagi"

// checkProjectDir 在部署前检查现有目录：不能在 Nagi 的数据目录里，也不能和本节点其他容器的目录
// 或现有目录重叠。面板只让管理员设置现有目录，这里是节点自己的一层，和导入时的检查一致。
func (m *Manager) checkProjectDir(ctx context.Context, s *nagiv1.ContainerSpec) error {
	dir := s.GetConfig().GetProjectDir()
	if dir == "" {
		return nil
	}
	real := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		real = r
	}
	for _, d := range m.ReservedDirs {
		if within(real, d) || within(d, real) {
			return invalid("现有目录 %s 是（或包含）Nagi 的数据目录", dir)
		}
	}
	for _, c := range m.List(ctx) {
		if c.Spec.GetProjectId() == s.GetProjectId() && c.Spec.GetName() == s.GetName() {
			continue
		}
		for _, d := range []string{c.Spec.GetDir(), c.Spec.GetConfig().GetProjectDir()} {
			if d != "" && (within(real, d) || within(d, real)) {
				return invalid("现有目录 %s 与此节点上容器 %s 的目录重叠", dir, c.Spec.GetName())
			}
		}
	}
	return nil
}

// takeOver 停掉 dir 里原来用 docker compose 运行的容器（不是 ours 这个 compose 项目的），改名让出容器名。
// 返回 commit（部署成功以后删除这些容器，卷和数据保留）和 undo（部署失败时删掉新起的容器，
// 把原来的改回原名、重新启动原来在运行的）。没有要接管的容器时两个都什么都不做。
func (m *Manager) takeOver(ctx context.Context, dir string, files compose.Files, onLine func(string)) (commit, undo func(), err error) {
	noop := func() {}
	list, err := m.docker.ComposeContainersIn(ctx, dir)
	if err != nil {
		return noop, noop, fmt.Errorf("查找目录里原来的容器: %w", err)
	}
	var old []docker.ComposeContainer
	projects := map[string]bool{}
	for _, c := range list {
		// Nagi 自己的容器（compose 项目名以 nagi- 开头）不接管：它们属于别的容器组，可能是别的项目。
		if c.Project != files.ProjectName && !strings.HasPrefix(c.Project, "nagi-") {
			old = append(old, c)
			projects[c.Project] = true
		}
	}
	if len(old) == 0 {
		return noop, noop, nil
	}
	names := make([]string, 0, len(projects))
	for p := range projects {
		names = append(names, p)
	}
	onLine(fmt.Sprintf("停止目录里原来的 compose 项目 %s（%d 个容器），由面板接管", strings.Join(names, "、"), len(old)))
	bg := context.WithoutCancel(ctx)
	var done []docker.ComposeContainer
	undo = func() {
		onLine("部署失败，恢复原来的容器")
		_ = compose.Down(bg, files, false, func(string) {})
		for _, c := range done {
			if err := m.docker.RenameContainer(bg, c.ID, c.Name); err != nil {
				onLine("警告：容器 " + c.Name + " 改回原名失败：" + err.Error())
				continue
			}
			if c.State == "running" {
				if err := m.docker.StartContainer(bg, c.ID); err != nil {
					onLine("警告：容器 " + c.Name + " 重新启动失败：" + err.Error())
				}
			}
		}
	}
	for _, c := range old {
		if c.State == "running" || c.State == "paused" || c.State == "restarting" {
			if err := m.docker.StopContainer(ctx, c.ID, 30); err != nil {
				undo()
				return noop, noop, fmt.Errorf("停止原来的容器 %s: %w", c.Name, err)
			}
		}
		if err := m.docker.RenameContainer(ctx, c.ID, c.Name+replacedSuffix); err != nil {
			// 停了还没改名的这一个也要恢复。
			done = append(done, docker.ComposeContainer{ID: c.ID, Name: c.Name, State: c.State})
			undo()
			return noop, noop, fmt.Errorf("给原来的容器 %s 改名: %w", c.Name, err)
		}
		done = append(done, c)
	}
	commit = func() {
		var errs []error
		for _, c := range done {
			if err := m.docker.RemoveStopped(bg, c.ID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.Name, err))
			}
		}
		if err := errors.Join(errs...); err != nil {
			onLine("警告：删除原来的容器失败（已停止，可以手动删除）：" + err.Error())
			return
		}
		onLine("已删除原来的容器，数据和卷保留")
	}
	return commit, undo, nil
}
