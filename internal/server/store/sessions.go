package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// 会话加固用到的查询（见 docs/panel.md#会话）。手写，不放进 queries.sql。

// SessionState 是一个会话的全部状态，连同成员的角色和访问范围。
type SessionState struct {
	ID               string
	TokenHash        string
	UserID           int64
	CreatedAt        int64
	ExpiresAt        int64
	ElevatedUntil    int64
	ElevatedIP       string
	RotatedAt        int64
	Challenge        string
	ChallengeAt      int64
	PublicKey        []byte
	KeyUntil         int64
	UserAgent        string
	LastIP           string
	LastSeen         int64
	TLSFP            string
	Nets             string
	Reverify         string
	ReverifyFailures int64
	Username         string
	Role             string
	AllProjects      int64
	AllNodes         int64
}

const sessionColumns = `s.id, s.token_hash, s.user_id, s.created_at, s.expires_at, s.elevated_until, s.elevated_ip, s.rotated_at,
	s.challenge, s.challenge_at, s.public_key, s.key_until, s.user_agent, s.last_ip, s.last_seen, s.tls_fp, s.nets, s.reverify,
	s.reverify_failures, u.username, u.role, u.all_projects, u.all_nodes`

func scanSession(r interface{ Scan(...any) error }) (SessionState, error) {
	var s SessionState
	err := r.Scan(&s.ID, &s.TokenHash, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.ElevatedUntil, &s.ElevatedIP, &s.RotatedAt,
		&s.Challenge, &s.ChallengeAt, &s.PublicKey, &s.KeyUntil, &s.UserAgent, &s.LastIP, &s.LastSeen, &s.TLSFP, &s.Nets, &s.Reverify,
		&s.ReverifyFailures, &s.Username, &s.Role, &s.AllProjects, &s.AllNodes)
	return s, err
}

// 令牌的状态。
const (
	TokenCurrent = "current"
	TokenPending = "pending"
	TokenRetired = "retired"
)

// SessionByToken 按令牌的哈希找会话：当前令牌、还没用过的新令牌，或者换下来的旧令牌（state、retiredAt 说明是哪一种）。
func (q *Queries) SessionByToken(ctx context.Context, hash string) (s SessionState, state string, retiredAt int64, err error) {
	s, err = scanSession(q.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, hash))
	if err == nil {
		return s, TokenCurrent, 0, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return s, "", 0, err
	}
	var id string
	if err = q.db.QueryRowContext(ctx, `SELECT session_id, state, retired_at FROM session_tokens WHERE token_hash = ?`, hash).Scan(&id, &state, &retiredAt); err != nil {
		return s, "", 0, err
	}
	s, err = q.SessionByID(ctx, id)
	return s, state, retiredAt, err
}

// SessionByID 按会话 ID 找会话。
func (q *Queries) SessionByID(ctx context.Context, id string) (SessionState, error) {
	return scanSession(q.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, id))
}

// sessionFields 是 UpdateSession 能改的列。
var sessionFields = map[string]bool{
	"rotated_at": true, "challenge": true, "challenge_at": true, "public_key": true, "key_until": true, "last_ip": true,
	"last_seen": true, "tls_fp": true, "nets": true, "elevated_until": true, "elevated_ip": true, "reverify": true,
	"reverify_failures": true, "user_agent": true, "expires_at": true,
}

// UpdateSession 修改会话的几列。
func (q *Queries) UpdateSession(ctx context.Context, id string, set map[string]any) error {
	if len(set) == 0 {
		return nil
	}
	var cols []string
	var args []any
	for k, v := range set {
		if !sessionFields[k] {
			return errors.New("unknown session field " + k)
		}
		cols = append(cols, k+" = ?")
		args = append(args, v)
	}
	_, err := q.db.ExecContext(ctx, `UPDATE sessions SET `+strings.Join(cols, ", ")+` WHERE id = ?`, append(args, id)...)
	return err
}

// AddPendingToken 记下发给浏览器、还没用过的新令牌。同一个会话只留一个。
func (q *Queries) AddPendingToken(ctx context.Context, sessionID, hash string, now int64) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM session_tokens WHERE session_id = ? AND state = 'pending'`, sessionID); err != nil {
		return err
	}
	_, err := q.db.ExecContext(ctx, `INSERT INTO session_tokens (token_hash, session_id, state, created_at) VALUES (?, ?, 'pending', ?)`, hash, sessionID, now)
	return err
}

// ConfirmToken 让新令牌转正：旧令牌记为换下来的，新令牌成为当前令牌。只在会话的当前令牌还是 oldHash 时生效。
func (q *Queries) ConfirmToken(ctx context.Context, sessionID, oldHash, newHash string, now int64) (bool, error) {
	res, err := q.db.ExecContext(ctx, `UPDATE sessions SET token_hash = ?, rotated_at = ?, challenge = '', challenge_at = 0 WHERE id = ? AND token_hash = ?`,
		newHash, now, sessionID, oldHash)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := q.db.ExecContext(ctx, `DELETE FROM session_tokens WHERE token_hash = ?`, newHash); err != nil {
		return true, err
	}
	_, err = q.db.ExecContext(ctx, `INSERT OR REPLACE INTO session_tokens (token_hash, session_id, state, created_at, retired_at) VALUES (?, ?, 'retired', ?, ?)`,
		oldHash, sessionID, now, now)
	return true, err
}

// DeleteSessionByID 删除会话和它的所有令牌。
func (q *Queries) DeleteSessionByID(ctx context.Context, id string) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM session_tokens WHERE session_id = ?`, id); err != nil {
		return err
	}
	_, err := q.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// PruneSessionTokens 删掉会话已经不在的令牌，和换下来超过 keep 秒的旧令牌。
func (q *Queries) PruneSessionTokens(ctx context.Context, before int64) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM session_tokens WHERE session_id NOT IN (SELECT id FROM sessions)`); err != nil {
		return err
	}
	_, err := q.db.ExecContext(ctx, `DELETE FROM session_tokens WHERE state = 'retired' AND retired_at < ?`, before)
	return err
}

// DeleteIdleSessions 删掉最近活动早于 before 的会话，返回删掉的会话（审计记录用）。
func (q *Queries) DeleteIdleSessions(ctx context.Context, before int64) ([]SessionState, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.last_seen < ?`, before)
	if err != nil {
		return nil, err
	}
	var out []SessionState
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	rows.Close()
	for _, s := range out {
		if err := q.DeleteSessionByID(ctx, s.ID); err != nil {
			return out, err
		}
	}
	return out, nil
}

// DeleteOtherSessionsByID 删掉成员除 keepID 以外的会话（改密码、开启两步验证以后）。
func (q *Queries) DeleteOtherSessionsByID(ctx context.Context, userID int64, keepID string) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM session_tokens WHERE session_id IN (SELECT id FROM sessions WHERE user_id = ? AND id != ?)`, userID, keepID); err != nil {
		return err
	}
	_, err := q.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id != ?`, userID, keepID)
	return err
}

// DeleteUserSessionByPrefix 删掉成员自己的一个会话（ID 的前 16 位，「已登录的设备」里的退出）。
func (q *Queries) DeleteUserSessionByPrefix(ctx context.Context, userID int64, prefix string) (int64, error) {
	var id string
	err := q.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE user_id = ? AND substr(id, 1, 16) = ?`, userID, prefix).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || len(prefix) != 16 {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	return 1, q.DeleteSessionByID(ctx, id)
}

// SeenNetwork 记下成员从这个网段登录，返回之前是否在 since 以后登录过这个网段，以及这个成员有没有任何登录记录。
func (q *Queries) SeenNetwork(ctx context.Context, userID int64, network string, now, since int64) (seen, any bool, err error) {
	var last int64
	err = q.db.QueryRowContext(ctx, `SELECT last_seen FROM login_networks WHERE user_id = ? AND network = ?`, userID, network).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, false, err
	}
	seen = err == nil && last >= since
	var n int64
	_ = q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_networks WHERE user_id = ? AND last_seen >= ?`, userID, since).Scan(&n)
	_, err = q.db.ExecContext(ctx, `INSERT INTO login_networks (user_id, network, last_seen) VALUES (?, ?, ?)
		ON CONFLICT (user_id, network) DO UPDATE SET last_seen = excluded.last_seen`, userID, network, now)
	return seen, n > 0, err
}
