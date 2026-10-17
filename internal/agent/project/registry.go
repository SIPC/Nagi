package project

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SIPC/nagi/internal/agent/compose"
)

// 私有仓库认证：每个项目有自己的 docker 配置目录 config/docker，部署时通过 DOCKER_CONFIG 使用，
// 不碰机器全局的 ~/.docker。两个项目用同一个仓库的不同账号也不会冲突。登录信息随项目目录走，
// 同步给备份节点和顶替节点。

func (p paths) dockerConfigDir() string { return filepath.Join(p.configDir(), "docker") }

// Registries 列出项目登录过的仓库。
func (m *Manager) Registries(ctx context.Context, id string) ([]string, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(paths{root: row.Dir}.dockerConfigDir(), "config.json"))
	if err != nil {
		return nil, nil
	}
	var cfg struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return nil, nil
	}
	out := make([]string, 0, len(cfg.Auths))
	for k := range cfg.Auths {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Login 登录私有仓库（会向仓库验证账号），登录信息写进项目的 docker 配置。
func (m *Manager) Login(ctx context.Context, id, registry, username, password string) ([]string, error) {
	registry = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(registry), "https://"), "/")
	if registry == "" || strings.ContainsAny(registry, " /") {
		return nil, invalid("请填写仓库地址，例如 ghcr.io、registry.example.com:5000（Docker Hub 填 docker.io）")
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return nil, invalid("请填写用户名和密码（或访问令牌）")
	}
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	dir := paths{root: row.Dir}.dockerConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := compose.Login(ctx, dir, registry, strings.TrimSpace(username), password); err != nil {
		return nil, invalid("%v", err)
	}
	return m.Registries(ctx, id)
}

// Logout 删除项目某个仓库的登录信息。
func (m *Manager) Logout(ctx context.Context, id, registry string) ([]string, error) {
	row, err := m.projectRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := compose.Logout(ctx, paths{root: row.Dir}.dockerConfigDir(), registry); err != nil {
		return nil, invalid("%v", err)
	}
	return m.Registries(ctx, id)
}
