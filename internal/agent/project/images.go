package project

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/distribution/reference"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 镜像更新提醒：每天检查一次各容器正在用的镜像有没有新版本。只向仓库查询标签当前的摘要（不下载镜像，
// 也不计入 Docker Hub 的拉取次数），和本地镜像的摘要比较。不自动升级，用户点"更新"时拉取新镜像并部署这个容器，
// 记为它的一个新版本，可以回滚。

type imageUpdates struct {
	mu      sync.Mutex
	updates map[string][]*nagiv1.ImageUpdate // 容器（uid）→ 有新版本的服务
	checked map[string]time.Time
}

func (m *Manager) imageStatus(projectID, container string, st *nagiv1.ContainerStatus) {
	m.images.mu.Lock()
	defer m.images.mu.Unlock()
	k := uid(projectID, container)
	st.ImageUpdates = m.images.updates[k]
	if t, ok := m.images.checked[k]; ok {
		st.ImagesCheckedAt = timestamppb.New(t)
	}
}

// clearImageUpdates 清掉容器的"有新版本"提醒（拉取了新镜像后、删除后）。
func (m *Manager) clearImageUpdates(projectID, container string) {
	m.images.mu.Lock()
	defer m.images.mu.Unlock()
	delete(m.images.updates, uid(projectID, container))
}

// registryAuth 从项目的 docker 配置里取出某个仓库的登录信息，编码成 Docker 接口要的格式。
func registryAuth(dockerConfig []byte, domain string) string {
	if len(dockerConfig) == 0 {
		return ""
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if json.Unmarshal(dockerConfig, &cfg) != nil {
		return ""
	}
	keys := []string{domain}
	if domain == "docker.io" {
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io")
	}
	for _, k := range keys {
		a, ok := cfg.Auths[k]
		if !ok {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(a.Auth)
		if err != nil {
			return ""
		}
		user, pass, _ := strings.Cut(string(raw), ":")
		js, _ := json.Marshal(map[string]string{"username": user, "password": pass, "serveraddress": k})
		return base64.URLEncoding.EncodeToString(js)
	}
	return ""
}

// CheckImages 检查容器正在用的镜像有没有新版本。回滚时固定了摘要的镜像、本地构建的镜像跳过。
func (m *Manager) CheckImages(ctx context.Context, projectID, container string) ([]*nagiv1.ImageUpdate, error) {
	c, err := m.Get(ctx, projectID, container)
	if err != nil {
		return nil, err
	}
	instances, err := m.docker.Instances(ctx, containerLabel(projectID, container))
	if err != nil {
		return nil, err
	}
	var out []*nagiv1.ImageUpdate
	seen := map[string]bool{}
	for _, in := range instances {
		if seen[in.Service] || strings.Contains(in.Image, "@") || strings.HasPrefix(in.Image, "sha256:") {
			continue
		}
		seen[in.Service] = true
		named, err := reference.ParseNormalizedNamed(in.Image)
		if err != nil {
			continue
		}
		named = reference.TagNameOnly(named)
		digests, err := m.docker.RepoDigests(ctx, in.ImageID)
		if err != nil {
			continue
		}
		local := ""
		for _, d := range digests {
			if dn, err := reference.ParseNormalizedNamed(d); err == nil && dn.Name() == named.Name() {
				if canon, ok := dn.(reference.Canonical); ok {
					local = canon.Digest().String()
				}
			}
		}
		if local == "" {
			continue // 本地构建的镜像，没有仓库摘要
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		remote, err := m.docker.RemoteDigest(cctx, named.String(), registryAuth(c.Spec.GetDockerConfig(), reference.Domain(named)))
		cancel()
		if err != nil || remote == "" || remote == local {
			continue
		}
		out = append(out, &nagiv1.ImageUpdate{Container: container, Service: in.Service, Image: reference.FamiliarString(named), CurrentDigest: local, LatestDigest: remote})
	}
	m.images.mu.Lock()
	if m.images.updates == nil {
		m.images.updates, m.images.checked = map[string][]*nagiv1.ImageUpdate{}, map[string]time.Time{}
	}
	k := uid(projectID, container)
	m.images.updates[k] = out
	m.images.checked[k] = time.Now()
	m.images.mu.Unlock()
	return out, nil
}

// CheckAllImages 检查本节点上所有部署过的容器，返回是否有容器的结果变了。
func (m *Manager) CheckAllImages(ctx context.Context) bool {
	changed := false
	for _, c := range m.List(ctx) {
		s := c.Spec
		if s.GetVersion() == 0 {
			continue
		}
		m.images.mu.Lock()
		before := len(m.images.updates[uid(s.GetProjectId(), s.GetName())])
		m.images.mu.Unlock()
		list, err := m.CheckImages(ctx, s.GetProjectId(), s.GetName())
		if err == nil && len(list) != before {
			changed = true
		}
	}
	return changed
}
