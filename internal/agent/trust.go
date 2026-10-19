package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// trustedProjects 记录各面板允许在本节点从副本启动时使用特权配置的项目，存在 identity/trusted-projects.json，
// 键是面板公钥指纹。只算本节点还信任的面板下发的。
type trustedProjects struct {
	path string
	mu   sync.Mutex
}

func (t *trustedProjects) load() map[string][]string {
	m := map[string][]string{}
	if b, err := os.ReadFile(t.path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (t *trustedProjects) set(panel string, ids []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.load()
	m[panel] = ids
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

// trusted 判断项目是否被某个仍然信任的面板允许。
func (t *trustedProjects) trusted(id string, stillTrusted func(panel string) bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for panel, ids := range t.load() {
		if stillTrusted(panel) && slices.Contains(ids, id) {
			return true
		}
	}
	return false
}

func newTrustedProjects(identityDir string) *trustedProjects {
	return &trustedProjects{path: filepath.Join(identityDir, "trusted-projects.json")}
}

func (s *agentService) SetTrustedProjects(_ context.Context, req *nagiv1.SetTrustedProjectsRequest) (*nagiv1.SetTrustedProjectsResponse, error) {
	if s.node.trusted == nil {
		return nil, errors.New("节点还没有准备好")
	}
	if err := s.node.trusted.set(s.panel, req.GetProjectIds()); err != nil {
		return nil, err
	}
	return &nagiv1.SetTrustedProjectsResponse{}, nil
}
