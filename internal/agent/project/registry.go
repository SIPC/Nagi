package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/SIPC/nagi/internal/agent/compose"
)

// 私有仓库认证：每个项目有自己的 docker 配置（存在面板上），部署时写进容器目录的 docker/，
// 通过 DOCKER_CONFIG 使用，不碰机器全局的 ~/.docker。两个项目用同一个仓库的不同账号也不会冲突。

// NormalizeRegistry 整理仓库地址，例如去掉 https:// 和末尾的 /。
func NormalizeRegistry(registry string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(registry), "https://"), "/")
}

// Login 登录私有仓库（会向仓库验证账号），返回只有这个仓库的 docker 配置（config.json）。
func (m *Manager) Login(ctx context.Context, registry, username, password string) ([]byte, error) {
	registry = NormalizeRegistry(registry)
	if registry == "" || strings.ContainsAny(registry, " /") || strings.HasPrefix(registry, "-") {
		return nil, invalid("请填写仓库地址，例如 ghcr.io、registry.example.com:5000（Docker Hub 填 docker.io）")
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return nil, invalid("请填写用户名和密码（或访问令牌）")
	}
	dir, err := os.MkdirTemp("", "nagi-login-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := compose.Login(ctx, dir, registry, strings.TrimSpace(username), password); err != nil {
		return nil, invalid("%v", err)
	}
	return os.ReadFile(filepath.Join(dir, "config.json"))
}
