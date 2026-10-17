package project

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/distribution/reference"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 镜像更新提醒：每天检查一次项目各成员正在用的镜像有没有新版本。只向仓库查询标签当前的摘要（不下载镜像，
// 也不计入 Docker Hub 的拉取次数），和本地镜像的摘要比较。不自动升级，用户点"更新"时拉取新镜像并部署这个成员，
// 记为它的一个新版本，可以回滚。

type imageUpdates struct {
	mu      sync.Mutex
	updates map[string][]*nagiv1.ImageUpdate // 项目 ID → 有新版本的服务（带成员名）
	checked map[string]time.Time
}

func (m *Manager) imageStatus(id string, st *nagiv1.ProjectStatus) {
	m.images.mu.Lock()
	defer m.images.mu.Unlock()
	st.ImageUpdates = m.images.updates[id]
	if t, ok := m.images.checked[id]; ok {
		st.ImagesCheckedAt = timestamppb.New(t)
	}
}

// clearImageUpdates 清掉成员的"有新版本"提醒（拉取了新镜像后）。
func (m *Manager) clearImageUpdates(id, member string) {
	m.images.mu.Lock()
	defer m.images.mu.Unlock()
	var keep []*nagiv1.ImageUpdate
	for _, u := range m.images.updates[id] {
		if u.GetMember() != member {
			keep = append(keep, u)
		}
	}
	m.images.updates[id] = keep
}

// registryAuth 从项目自己的 docker 配置里取出某个仓库的登录信息，编码成 Docker 接口要的格式。
func registryAuth(p paths, domain string) string {
	b, err := os.ReadFile(filepath.Join(p.dockerConfigDir(), "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if json.Unmarshal(b, &cfg) != nil {
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

// CheckImages 检查项目正在用的镜像有没有新版本。回滚时固定了摘要的镜像、本地构建的镜像跳过。
func (m *Manager) CheckImages(ctx context.Context, id string) ([]*nagiv1.ImageUpdate, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	p := paths{root: row.Dir}
	containers, err := m.docker.Containers(ctx, projectLabel(id))
	if err != nil {
		return nil, err
	}
	var out []*nagiv1.ImageUpdate
	seen := map[string]bool{}
	for _, c := range containers {
		key := c.Member + "/" + c.Service
		if seen[key] || strings.Contains(c.Image, "@") || strings.HasPrefix(c.Image, "sha256:") {
			continue
		}
		seen[key] = true
		named, err := reference.ParseNormalizedNamed(c.Image)
		if err != nil {
			continue
		}
		named = reference.TagNameOnly(named)
		digests, err := m.docker.RepoDigests(ctx, c.ImageID)
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
		remote, err := m.docker.RemoteDigest(cctx, named.String(), registryAuth(p, reference.Domain(named)))
		cancel()
		if err != nil || remote == "" || remote == local {
			continue
		}
		out = append(out, &nagiv1.ImageUpdate{Member: c.Member, Service: c.Service, Image: reference.FamiliarString(named), CurrentDigest: local, LatestDigest: remote})
	}
	m.images.mu.Lock()
	if m.images.updates == nil {
		m.images.updates, m.images.checked = map[string][]*nagiv1.ImageUpdate{}, map[string]time.Time{}
	}
	m.images.updates[id] = out
	m.images.checked[id] = time.Now()
	m.images.mu.Unlock()
	return out, nil
}

// CheckAllImages 检查本节点所有已部署的项目，返回是否有项目的结果变了。
func (m *Manager) CheckAllImages(ctx context.Context) bool {
	rows, err := m.nodeQ.ListProjects(ctx)
	if err != nil {
		return false
	}
	changed := false
	for _, r := range rows {
		m.images.mu.Lock()
		before := len(m.images.updates[r.ID])
		m.images.mu.Unlock()
		list, err := m.CheckImages(ctx, r.ID)
		if err == nil && len(list) != before {
			changed = true
		}
	}
	return changed
}
