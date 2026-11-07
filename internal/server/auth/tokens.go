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
	if now.Unix()-t.LastUsedAt >= int64(tokenTouchInterval/time.Second) || t.LastIP != ip {
		// 备用面板上写不进去也没关系：下一次同步会被主面板的数据覆盖。
		_ = s.q.TouchAccessToken(ctx, t.ID, now.Unix(), ip)
	}
	u := User{
		ID: row.ID, Name: row.Username, Role: row.Role,
		LimitProjects: row.AllProjects == 0, LimitNodes: row.AllNodes == 0,
		TokenID: t.ID, Via: "mcp:" + t.Name,
		// 只读成员本来就不能修改、不能打开终端，令牌上的选项也不放开。
		AllowWrite: t.AllowWrite && row.Role != RoleViewer,
		AllowExec:  t.AllowExec && row.Role != RoleViewer,
		// 节点命令等于节点的 root，只给管理员；成员被降级后令牌上的这一项随之失效。
		AllowNodeExec: t.AllowNodeExec && row.Role == RoleAdmin,
	}
	return u, t, nil
}
