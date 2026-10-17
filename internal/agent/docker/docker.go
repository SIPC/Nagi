// Package docker 通过 Docker Engine API 读取容器状态、日志和事件。
// 部署本身走 docker compose 命令（见 compose 包），这里只做读取。
package docker

import (
	"bufio"
	"context"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
)

// Client 包装 Docker Engine API 客户端。
type Client struct {
	c *client.Client
}

// New 按环境变量（DOCKER_HOST 等）连接 Docker，并自动协商 API 版本。
func New() (*Client, error) {
	c, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Close 关闭客户端。
func (d *Client) Close() error { return d.c.Close() }

// Version 返回 Docker 引擎版本。
func (d *Client) Version(ctx context.Context) (string, error) {
	v, err := d.c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return "", err
	}
	return v.Version, nil
}

// NetworkSubnets 返回 Docker 里所有网络已占用的网段，分配项目网段时要避开。
func (d *Client) NetworkSubnets(ctx context.Context) ([]netip.Prefix, error) {
	list, err := d.c.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, n := range list.Items {
		for _, c := range n.IPAM.Config {
			if c.Subnet.IsValid() {
				out = append(out, c.Subnet)
			}
		}
	}
	return out, nil
}

// Container 是一个 compose 服务对应的容器。
type Container struct {
	ID      string
	Service string
	Image   string
	State   string
	Status  string
	Health  string
}

// ProjectContainers 列出某个 compose 项目的全部容器（包括已停止的）。
func (d *Client) ProjectContainers(ctx context.Context, composeProject string) ([]Container, error) {
	list, err := d.c.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelProject+"="+composeProject),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list.Items))
	for _, c := range list.Items {
		out = append(out, Container{
			ID:      c.ID,
			Service: c.Labels[labelService],
			Image:   c.Image,
			State:   string(c.State),
			Status:  c.Status,
			Health:  health(c.Health),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// health 返回健康检查状态；没有健康检查时返回空字符串。
func health(h *container.HealthSummary) string {
	if h == nil || h.Status == container.NoHealthcheck {
		return ""
	}
	return string(h.Status)
}

// LogLine 是一行容器日志。
type LogLine struct {
	Service string
	Stream  string
	Text    string
	Time    time.Time
}

// Logs 读取一个容器的日志，每一行调用一次 onLine，直到结束或 ctx 取消。
func (d *Client) Logs(ctx context.Context, c Container, tail int, follow bool, onLine func(LogLine)) error {
	info, err := d.c.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	tailStr := "all"
	if tail > 0 {
		tailStr = strconv.Itoa(tail)
	}
	rc, err := d.c.ContainerLogs(ctx, c.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tailStr,
		Timestamps: true,
	})
	if err != nil {
		return err
	}
	defer rc.Close()

	emit := func(stream string) io.Writer {
		pr, pw := io.Pipe()
		go func() {
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				ts, text := splitTimestamp(sc.Text())
				onLine(LogLine{Service: c.Service, Stream: stream, Text: text, Time: ts})
			}
			io.Copy(io.Discard, pr)
		}()
		return pw
	}

	if info.Container.Config != nil && info.Container.Config.Tty {
		w := emit("stdout")
		_, err = io.Copy(w, rc)
		w.(*io.PipeWriter).Close()
	} else {
		stdout, stderr := emit("stdout"), emit("stderr")
		_, err = stdcopy.StdCopy(stdout, stderr, rc)
		stdout.(*io.PipeWriter).Close()
		stderr.(*io.PipeWriter).Close()
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// 开启 Timestamps 后，每行以 RFC3339Nano 时间戳加一个空格开头。
func splitTimestamp(line string) (time.Time, string) {
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		return time.Time{}, line
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, line
	}
	return t, rest
}

// WatchProjects 监听所有 Nagi 管理的容器的事件，有变化时调用 onChange。
// 出错（例如 Docker 重启）后会自动重连，直到 ctx 取消。
func (d *Client) WatchProjects(ctx context.Context, labelKey string, onChange func()) {
	for ctx.Err() == nil {
		res := d.c.Events(ctx, client.EventsListOptions{
			Filters: make(client.Filters).
				Add("type", string(events.ContainerEventType)).
				Add("label", labelKey),
		})
		msgs, errs := res.Messages, res.Err
	loop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-msgs:
				onChange()
			case <-errs:
				break loop
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
