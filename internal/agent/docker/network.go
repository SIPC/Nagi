package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// 项目内网由节点自己创建，容器组的容器由节点用 network connect 接入（只带指定的别名）。
// compose 会把服务原名注册到它加入的每个网络上，所以容器组不在 compose 里加入项目内网。

// EnsureNetwork 网络不存在时按指定网段创建（bridge）。
func (d *Client) EnsureNetwork(ctx context.Context, name, subnet string, labels map[string]string) error {
	if _, err := d.c.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	pfx, err := netip.ParsePrefix(subnet)
	if err != nil {
		return fmt.Errorf("网段 %q 无效: %w", subnet, err)
	}
	_, err = d.c.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
		IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: pfx}}},
		Labels: labels,
	})
	return err
}

// RemoveNetwork 删除网络，不存在时什么都不做。还有容器连着时返回错误。
func (d *Client) RemoveNetwork(ctx context.Context, name string) error {
	_, err := d.c.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// NetworkAliases 返回容器连着的每个网络，以及它在这个网络里的别名。
func (d *Client) NetworkAliases(ctx context.Context, containerID string) (map[string][]string, error) {
	info, err := d.c.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	if ns := info.Container.NetworkSettings; ns != nil {
		for name, ep := range ns.Networks {
			if ep != nil {
				out[name] = ep.Aliases
			} else {
				out[name] = nil
			}
		}
	}
	return out, nil
}

// Connect 把容器接入网络，带上这些别名。
func (d *Client) Connect(ctx context.Context, networkName, containerID string, aliases []string) error {
	_, err := d.c.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{
		Container:      containerID,
		EndpointConfig: &network.EndpointSettings{Aliases: aliases},
	})
	return err
}

// Disconnect 把容器从网络断开。容器或网络已经不在时什么都不做。
func (d *Client) Disconnect(ctx context.Context, networkName, containerID string) error {
	_, err := d.c.NetworkDisconnect(ctx, networkName, client.NetworkDisconnectOptions{Container: containerID, Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// IsNotFound 判断 Docker 返回的错误是不是"不存在"。
func IsNotFound(err error) bool {
	var nf interface{ NotFound() }
	return cerrdefs.IsNotFound(err) || errors.As(err, &nf)
}
