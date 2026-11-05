package docker

import (
	"context"
	"strings"

	"github.com/moby/moby/client"
)

// 现有目录（见 docs/agent.md#现有目录）：找出用户自己用 docker compose 在某个目录里起的容器，
// 面板接管时先停掉、改名，接管成功再删除，失败就改回原名重新启动。

const (
	labelWorkingDir = "com.docker.compose.project.working_dir"
	labelVolume     = "com.docker.compose.volume"
)

// ComposeContainer 是 docker compose 起的一个容器。
type ComposeContainer struct {
	ID, Name, Project, Service, State string
}

// ComposeContainersIn 返回在 dir 里用 docker compose 起的容器（看 compose 记在容器上的工作目录），包括停止的。
func (d *Client) ComposeContainersIn(ctx context.Context, dir string) ([]ComposeContainer, error) {
	list, err := d.c.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelWorkingDir+"="+dir),
	})
	if err != nil {
		return nil, err
	}
	var out []ComposeContainer
	for _, c := range list.Items {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, ComposeContainer{ID: c.ID, Name: name, Project: c.Labels[labelProject], Service: c.Labels[labelService], State: string(c.State)})
	}
	return out, nil
}

// ComposeVolumes 返回 compose 项目的命名卷：compose 里的名字到 Docker 里的名字。
func (d *Client) ComposeVolumes(ctx context.Context, project string) (map[string]string, error) {
	list, err := d.c.VolumeList(ctx, client.VolumeListOptions{Filters: make(client.Filters).Add("label", labelProject+"="+project)})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, v := range list.Items {
		if short := v.Labels[labelVolume]; short != "" {
			out[short] = v.Name
		}
	}
	return out, nil
}

// StopContainer 停止一个容器，最多等 timeout 秒再强制结束。
func (d *Client) StopContainer(ctx context.Context, id string, timeout int) error {
	_, err := d.c.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

// RenameContainer 给容器改名。
func (d *Client) RenameContainer(ctx context.Context, id, name string) error {
	_, err := d.c.ContainerRename(ctx, id, client.ContainerRenameOptions{NewName: name})
	return err
}

// RemoveStopped 删除一个已停止的容器，不删它的卷。
func (d *Client) RemoveStopped(ctx context.Context, id string) error {
	_, err := d.c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{})
	return err
}
