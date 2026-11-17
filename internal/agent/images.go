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

	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// imageCheckLoop 每天检查一次所有容器的镜像有没有新版本（启动 5 分钟后第一次）。
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

func (s *agentService) CheckImages(ctx context.Context, req *nagiv1.ContainerRef) (*nagiv1.CheckImagesResponse, error) {
	list, err := s.projects.CheckImages(ctx, req.GetProjectId(), req.GetContainer())
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

// SetMirrors 写入加速地址，保留 daemon.json 里的其他配置，然后让 Docker 重新加载配置（见 applyMirrors），
// 不重启 Docker，容器不受影响。重新加载不了时在响应里说明，要重启 Docker 才生效，Nagi 不会自己重启。
func (s *agentService) SetMirrors(ctx context.Context, req *nagiv1.Mirrors) (*nagiv1.Mirrors, error) {
	var list []string
	for _, m := range req.GetMirrors() {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		u, err := url.Parse(m)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, status.Errorf(codes.InvalidArgument, "加速地址须为以 http:// 或 https:// 开头的完整地址：%s", m)
		}
		// 和 Docker 的检查一样（registry.ValidateMirror）：不合规的地址会让 Docker 拒绝整份配置，重启时起不来。
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return nil, status.Errorf(codes.InvalidArgument, "加速地址不能包含账号、查询参数或 #：%s", u.Redacted())
		}
		list = append(list, m)
	}
	cfg, err := readDaemonJSON()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	switch _, had := cfg["registry-mirrors"]; {
	case len(list) > 0:
		cfg["registry-mirrors"] = list
	case had:
		// 清空时写空列表而不是删掉这一项：Docker 重新加载配置时只改文件里有的项，删掉的话它会接着用原来的地址。
		cfg["registry-mirrors"] = []string{}
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
	out, err := s.node.mirrors(ctx)
	if err != nil {
		return nil, err
	}
	active, err := s.node.applyMirrors(ctx, hostDockerReloader, list)
	if err != nil {
		s.node.log.Warn("已写入镜像加速地址，但没能让 Docker 重新加载配置，重启 Docker 后生效", "mirrors", list, "err", err)
		out.ReloadError = i18n.Localize(i18n.Zh, err) // 底层原因只记日志
		return out, nil
	}
	s.node.log.Info("已写入镜像加速地址，Docker 已在使用", "mirrors", list)
	out.Active, out.Reloaded = active, true
	return out, nil
}
