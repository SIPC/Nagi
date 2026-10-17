package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// imageCheckLoop 每天检查一次所有项目的镜像有没有新版本（启动 5 分钟后第一次）。
func (a *agent) imageCheckLoop(ctx context.Context) {
	wait := 5 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if a.projects.CheckAllImages(ctx) {
			a.changed() // 结果变了，重新上报给面板
		}
		wait = 24 * time.Hour
	}
}

func (s *agentService) CheckImages(ctx context.Context, req *nagiv1.ReplicaRef) (*nagiv1.CheckImagesResponse, error) {
	list, err := s.projects.CheckImages(ctx, req.GetProjectId())
	if err != nil {
		return nil, toStatus(err)
	}
	s.changed()
	return &nagiv1.CheckImagesResponse{Updates: list}, nil
}

// ---- 镜像加速 ----

// daemonJSON 是 Docker 的配置文件。只有用户在面板上设置了加速地址，Nagi 才会改它。
var daemonJSON = "/etc/docker/daemon.json"

func readDaemonJSON() (map[string]any, error) {
	cfg := map[string]any{}
	b, err := os.ReadFile(daemonJSON)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, errors.New(daemonJSON + " 不是有效的 JSON，请先手动修正")
	}
	return cfg, nil
}

func (a *agent) mirrors(ctx context.Context) (*nagiv1.Mirrors, error) {
	cfg, err := readDaemonJSON()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	out := &nagiv1.Mirrors{}
	if list, ok := cfg["registry-mirrors"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				out.Mirrors = append(out.Mirrors, s)
			}
		}
	}
	out.Active, _ = a.docker.Mirrors(ctx)
	return out, nil
}

func (s *agentService) GetMirrors(ctx context.Context, _ *nagiv1.GetMirrorsRequest) (*nagiv1.Mirrors, error) {
	return s.node.mirrors(ctx)
}

// SetMirrors 写入加速地址，保留 daemon.json 里的其他配置。需要重启 Docker 才生效，Nagi 不会自己重启。
func (s *agentService) SetMirrors(ctx context.Context, req *nagiv1.Mirrors) (*nagiv1.Mirrors, error) {
	var list []string
	for _, m := range req.GetMirrors() {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		u, err := url.Parse(m)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, status.Errorf(codes.InvalidArgument, "加速地址必须是 http(s):// 开头的完整地址：%s", m)
		}
		list = append(list, m)
	}
	cfg, err := readDaemonJSON()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if len(list) == 0 {
		delete(cfg, "registry-mirrors")
	} else {
		cfg["registry-mirrors"] = list
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Dir(daemonJSON), 0o755); err != nil {
		return nil, err
	}
	tmp := daemonJSON + ".nagi-tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Rename(tmp, daemonJSON); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.node.log.Info("已写入镜像加速地址，重启 Docker 后生效", "mirrors", list)
	return s.node.mirrors(ctx)
}
