package store

// 访问令牌、OAuth 客户端和 MCP 用到的活动记录查询（见 docs/mcp.md）。手写的：sqlc 在这套表结构上生成失败，
// 这些查询不放进 queries.sql，免得以后能生成时重复。

import (
	"context"
	"database/sql"
)

// AccessToken 是一个访问令牌：个人访问令牌，或者一次 OAuth 授权。
type AccessToken struct {
	ID               int64
	UserID           int64
	Name             string
	Source           string
	ClientID         string
	TokenHash        string
	RefreshHash      string
	PrevRefreshHash  string
	AllowWrite       bool
	AllowExec        bool
	CreatedAt        int64
	ExpiresAt        int64
	RefreshExpiresAt int64
	LastUsedAt       int64
	LastIP           string
}

const accessTokenColumns = `id, user_id, name, source, client_id, token_hash, refresh_hash, prev_refresh_hash,
	allow_write, allow_exec, created_at, expires_at, refresh_expires_at, last_used_at, last_ip`

func scanAccessToken(row interface{ Scan(...any) error }) (AccessToken, error) {
	var t AccessToken
	var write, exec int64
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Source, &t.ClientID, &t.TokenHash, &t.RefreshHash, &t.PrevRefreshHash,
		&write, &exec, &t.CreatedAt, &t.ExpiresAt, &t.RefreshExpiresAt, &t.LastUsedAt, &t.LastIP)
	t.AllowWrite, t.AllowExec = write != 0, exec != 0
	return t, err
}

func flag(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// InsertAccessToken 保存新令牌，返回 ID。
func (q *Queries) InsertAccessToken(ctx context.Context, t AccessToken) (int64, error) {
	res, err := q.db.ExecContext(ctx, `INSERT INTO api_tokens (user_id, name, source, client_id, token_hash, refresh_hash,
		allow_write, allow_exec, created_at, expires_at, refresh_expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.UserID, t.Name, t.Source, t.ClientID, t.TokenHash, t.RefreshHash, flag(t.AllowWrite), flag(t.AllowExec),
		t.CreatedAt, t.ExpiresAt, t.RefreshExpiresAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (q *Queries) accessTokenWhere(ctx context.Context, where string, arg any) (AccessToken, error) {
	return scanAccessToken(q.db.QueryRowContext(ctx, "SELECT "+accessTokenColumns+" FROM api_tokens WHERE "+where, arg))
}

// AccessTokenByHash 按令牌的哈希查找。
func (q *Queries) AccessTokenByHash(ctx context.Context, hash string) (AccessToken, error) {
	return q.accessTokenWhere(ctx, "token_hash = ?", hash)
}

// AccessTokenByRefresh 按刷新令牌的哈希查找。
func (q *Queries) AccessTokenByRefresh(ctx context.Context, hash string) (AccessToken, error) {
	if hash == "" {
		return AccessToken{}, sql.ErrNoRows
	}
	return q.accessTokenWhere(ctx, "refresh_hash = ?", hash)
}

// AccessTokenByPrevRefresh 按上一个刷新令牌的哈希查找（刷新令牌被重复使用）。
func (q *Queries) AccessTokenByPrevRefresh(ctx context.Context, hash string) (AccessToken, error) {
	if hash == "" {
		return AccessToken{}, sql.ErrNoRows
	}
	return q.accessTokenWhere(ctx, "prev_refresh_hash = ?", hash)
}

// RotateAccessToken 换一对新的访问令牌和刷新令牌，旧的刷新令牌记为上一个。
func (q *Queries) RotateAccessToken(ctx context.Context, id int64, tokenHash string, expiresAt int64, refreshHash, prevRefreshHash string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE api_tokens SET token_hash = ?, expires_at = ?, refresh_hash = ?, prev_refresh_hash = ? WHERE id = ?`,
		tokenHash, expiresAt, refreshHash, prevRefreshHash, id)
	return err
}

// TouchAccessToken 记下最后使用的时间和地址。
func (q *Queries) TouchAccessToken(ctx context.Context, id, at int64, ip string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ?, last_ip = ? WHERE id = ?`, at, ip, id)
	return err
}

// ListAccessTokens 列出成员的令牌，新的在前。
func (q *Queries) ListAccessTokens(ctx context.Context, userID int64) ([]AccessToken, error) {
	rows, err := q.db.QueryContext(ctx, "SELECT "+accessTokenColumns+" FROM api_tokens WHERE user_id = ? ORDER BY id DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessToken
	for rows.Next() {
		t, err := scanAccessToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountAccessTokens 返回成员的令牌数。
func (q *Queries) CountAccessTokens(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, "SELECT count(*) FROM api_tokens WHERE user_id = ?", userID).Scan(&n)
	return n, err
}

// DeleteAccessToken 吊销一个令牌，返回删掉的条数。
func (q *Queries) DeleteAccessToken(ctx context.Context, id int64) (int64, error) {
	res, err := q.db.ExecContext(ctx, "DELETE FROM api_tokens WHERE id = ?", id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteUserAccessTokens 吊销成员的所有令牌（管理员重置了密码）。
func (q *Queries) DeleteUserAccessTokens(ctx context.Context, userID int64) error {
	_, err := q.db.ExecContext(ctx, "DELETE FROM api_tokens WHERE user_id = ?", userID)
	return err
}

// DeleteExpiredAccessTokens 删除过期的令牌：个人令牌看 expires_at，OAuth 看整个授权的有效期。
func (q *Queries) DeleteExpiredAccessTokens(ctx context.Context, now int64) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE
		(source = 'personal' AND expires_at > 0 AND expires_at < ?1) OR
		(source = 'oauth' AND refresh_expires_at > 0 AND refresh_expires_at < ?1)`, now)
	return err
}

// OAuthClient 是一个动态注册的 OAuth 客户端。
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs string
	CreatedAt    int64
	AuthorizedAt int64
}

// InsertOAuthClient 保存新注册的客户端。
func (q *Queries) InsertOAuthClient(ctx context.Context, c OAuthClient) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		c.ID, c.Name, c.RedirectURIs, c.CreatedAt)
	return err
}

// GetOAuthClient 按 ID 查找客户端。
func (q *Queries) GetOAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	var c OAuthClient
	err := q.db.QueryRowContext(ctx, `SELECT id, name, redirect_uris, created_at, authorized_at FROM oauth_clients WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.RedirectURIs, &c.CreatedAt, &c.AuthorizedAt)
	return c, err
}

// MarkOAuthClientAuthorized 记下客户端第一次被授权，之后不再被当作没用的注册清掉。
func (q *Queries) MarkOAuthClientAuthorized(ctx context.Context, id string, at int64) error {
	_, err := q.db.ExecContext(ctx, `UPDATE oauth_clients SET authorized_at = ? WHERE id = ? AND authorized_at = 0`, at, id)
	return err
}

// CountOAuthClients 返回已注册的客户端数。
func (q *Queries) CountOAuthClients(ctx context.Context) (int64, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, "SELECT count(*) FROM oauth_clients").Scan(&n)
	return n, err
}

// DeleteStaleOAuthClients 删除注册后一直没被授权的客户端。
func (q *Queries) DeleteStaleOAuthClients(ctx context.Context, before int64) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM oauth_clients WHERE authorized_at = 0 AND created_at < ?`, before)
	return err
}

// ListProjectActivity 返回项目的活动记录（见 ListProjectEvents），新的在前。container 为空表示所有容器；
// 部署的目标可能是逗号分隔的几个容器，和 ListContainerActivity 一样匹配。
func (q *Queries) ListProjectActivity(ctx context.Context, projectID, container string, before int64, failedOnly bool, limit int64) ([]Activity, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT id, created_at, kind, actor, action, project_id, target, detail, failed, via FROM activity
		WHERE project_id = ?1
		AND (?2 = '' OR target = ?2 OR ',' || replace(target, ' ', '') || ',' LIKE '%,' || ?2 || ',%')
		AND (?3 = 0 OR id < ?3) AND (?4 = 0 OR failed = 1)
		ORDER BY id DESC LIMIT ?5`, projectID, container, before, flag(failedOnly), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var i Activity
		if err := rows.Scan(&i.ID, &i.CreatedAt, &i.Kind, &i.Actor, &i.Action, &i.ProjectID, &i.Target, &i.Detail, &i.Failed, &i.Via); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
