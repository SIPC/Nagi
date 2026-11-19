package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/server/store"
)

// 访问令牌（见 docs/mcp.md#登录）：个人访问令牌和 OAuth 授权发出的令牌都存在 api_tokens 表里，只存哈希。

const (
	// TokenPrefix 是访问令牌的前缀，方便认出来（例如扫描泄露的密钥）。
	TokenPrefix = "nagi_"
	// TokenSourcePersonal、TokenSourceOAuth 是令牌的来源。
	TokenSourcePersonal = "personal"
	TokenSourceOAuth    = "oauth"
	// tokenTouchInterval 内重复使用不再更新最后使用时间，免得每次调用都写数据库。
	tokenTouchInterval = time.Minute
)

// ErrInvalidToken 表示令牌不存在、已吊销或已过期。
var ErrInvalidToken = i18n.New("访问令牌不正确或已过期，请重新创建令牌")

// NewSecret 生成一个新的令牌（或刷新令牌、授权码）：前缀加 32 字节随机数。
func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// TokenUser 核对访问令牌，返回令牌对应的成员（角色和访问范围按成员当前的设置）。
func (s *Service) TokenUser(ctx context.Context, token, ip string) (User, store.AccessToken, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return User{}, store.AccessToken{}, ErrInvalidToken
	}
	t, err := s.q.AccessTokenByHash(ctx, HashToken(token))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, t, ErrInvalidToken
	} else if err != nil {
		return User{}, t, err
	}
	now := time.Now()
	if t.ExpiresAt > 0 && t.ExpiresAt < now.Unix() {
		return User{}, t, ErrInvalidToken
	}
	row, err := s.q.GetUserByID(ctx, t.UserID)
	if err != nil {
		return User{}, t, ErrInvalidToken
	}
	// 网段限制（见 docs/mcp.md#网段）：不在范围内就当令牌无效，并告警（令牌很可能泄露了）。
	if t.Networks != "" {
		prefixes, err := ParseNetworks(strings.Split(t.Networks, "\n"))
		if err == nil && len(prefixes) > 0 && !prefixesContain(prefixes, ip) {
			s.tokenNetworkDenied(ctx, t, row.Username, ip, "token")
			return User{}, t, ErrInvalidToken
		}
	}
	// 管理员登录地址（见 docs/panel.md#管理员登录地址）对管理员的令牌同样生效，包括 OAuth 授权的：
	// 令牌泄露以后，别人不能从别的地方拿它调用 MCP、执行节点命令。
	if row.Role == RoleAdmin && !s.adminAllowed(ctx, ip) {
		s.tokenNetworkDenied(ctx, t, row.Username, ip, "admin")
		return User{}, t, ErrInvalidToken
	}
	client := clipName(RequestMeta(ctx).Client)
	if now.Unix()-t.LastUsedAt >= int64(tokenTouchInterval/time.Second) || t.LastIP != ip || t.LastClient != client {
		// 备用面板上写不进去也没关系：下一次同步会被主面板的数据覆盖。
		_ = s.q.TouchAccessToken(ctx, t.ID, now.Unix(), ip, client)
	}
	s.checkTokenNetwork(ctx, t, row.Username, ip, client)
	perms, err := s.q.RolePerms(ctx, row.RoleID)
	if err != nil {
		return User{}, t, err
	}
	u := User{ID: row.ID, Name: row.Username, TokenID: t.ID, Via: "mcp:" + t.Name}
	u.applyRole(row.Role, row.RoleID, perms, row.AllProjects, row.AllNodes, row.NodesFollow)
	u.applyToken(t.AllowWrite, t.AllowExec, t.AllowNodeExec, t.AllowPanel)
	return u, t, nil
}
