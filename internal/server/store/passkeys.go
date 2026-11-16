package store

import (
	"context"
)

// 通行密钥的查询（见 docs/panel.md#通行密钥）。手写，不放进 queries.sql。

// Passkey 是一个通行密钥。
type Passkey struct {
	ID           int64
	UserID       int64
	CredentialID []byte
	PublicKey    []byte
	SignCount    int64
	Name         string
	RPID         string
	CreatedAt    int64
	LastUsedAt   int64
}

const passkeyColumns = `id, user_id, credential_id, public_key, sign_count, name, rp_id, created_at, last_used_at`

func scanPasskey(row interface{ Scan(...any) error }) (Passkey, error) {
	var p Passkey
	err := row.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.PublicKey, &p.SignCount, &p.Name, &p.RPID, &p.CreatedAt, &p.LastUsedAt)
	return p, err
}

// PasskeyByCredential 按凭据 ID 找通行密钥。
func (q *Queries) PasskeyByCredential(ctx context.Context, credentialID []byte) (Passkey, error) {
	return scanPasskey(q.db.QueryRowContext(ctx, `SELECT `+passkeyColumns+` FROM passkeys WHERE credential_id = ?`, credentialID))
}

// UserPasskeys 列出成员的通行密钥，先添加的在前。
func (q *Queries) UserPasskeys(ctx context.Context, userID int64) ([]Passkey, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+passkeyColumns+` FROM passkeys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddPasskey 保存新的通行密钥。
func (q *Queries) AddPasskey(ctx context.Context, p Passkey) (int64, error) {
	res, err := q.db.ExecContext(ctx, `INSERT INTO passkeys (user_id, credential_id, public_key, sign_count, name, rp_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, p.UserID, p.CredentialID, p.PublicKey, p.SignCount, p.Name, p.RPID, p.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UsePasskey 记下一次登录：签名计数只能变大（计数为 0 的验证器不计数），返回是否更新了。
func (q *Queries) UsePasskey(ctx context.Context, id, signCount, now int64) (bool, error) {
	res, err := q.db.ExecContext(ctx, `UPDATE passkeys SET sign_count = ?, last_used_at = ?
		WHERE id = ? AND (sign_count = 0 AND ? = 0 OR sign_count < ?)`, signCount, now, id, signCount, signCount)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeletePasskey 删除成员自己的通行密钥，返回删掉的名称（没有时为空）。
func (q *Queries) DeletePasskey(ctx context.Context, userID, id int64) (string, error) {
	var name string
	err := q.db.QueryRowContext(ctx, `DELETE FROM passkeys WHERE id = ? AND user_id = ? RETURNING name`, id, userID).Scan(&name)
	return name, err
}
