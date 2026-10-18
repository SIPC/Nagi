package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"io"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// 跨节点共享网络的代理容器（见 docs/agent.md#跨节点连接共享网络）：用节点本地生成的空镜像，
// 运行挂进去的 nagi 本身（nagi link-proxy），不用拉取任何镜像。

// LinkImage 是代理容器用的空镜像。
const LinkImage = "nagi-link:empty"

// EnsureLinkImage 本地没有空镜像时用一个空的 tar 包导入一个。
func (d *Client) EnsureLinkImage(ctx context.Context) error {
	if _, err := d.c.ImageInspect(ctx, LinkImage); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.Close(); err != nil {
		return err
	}
	r, err := d.c.ImageImport(ctx, client.ImageImportSource{Source: &buf, SourceName: "-"}, LinkImage, client.ImageImportOptions{})
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(io.Discard, r)
	return err
}

// LinkProxySpec 描述一个代理容器。
type LinkProxySpec struct {
	Name    string
	Network string // Docker 网络名
	Alias   string // 在网络里的名字
	Exe     string // 主机上 nagi 可执行文件的路径，挂进容器作为 /nagi
	SockDir string // 主机上放 unix socket 的目录，挂进容器作为 /run/nagi
	Env     []string
	Labels  map[string]string
}

// RunLinkProxy 创建并启动一个代理容器。
func (d *Client) RunLinkProxy(ctx context.Context, s LinkProxySpec) error {
	res, err := d.c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: s.Name,
		Config: &container.Config{
			Image:      LinkImage,
			Entrypoint: []string{"/nagi", "link-proxy"},
			Env:        s.Env,
			Labels:     s.Labels,
		},
		HostConfig: &container.HostConfig{
			Binds:         []string{s.Exe + ":/nagi:ro", s.SockDir + ":/run/nagi"},
			NetworkMode:   container.NetworkMode(s.Network),
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			LogConfig:     container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m", "max-file": "1"}},
		},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			s.Network: {Aliases: []string{s.Alias}},
		}},
	})
	if err != nil {
		return err
	}
	if _, err := d.c.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		_ = d.RemoveContainer(ctx, res.ID)
		return err
	}
	return nil
}

// RemoveContainer 强制删除容器，不存在时什么都不做。
func (d *Client) RemoveContainer(ctx context.Context, id string) error {
	_, err := d.c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}
