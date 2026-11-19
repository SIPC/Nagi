package store

// 加入令牌的查询。手写的，理由同 tokens.go。

import "context"

// ActiveInstallTokens 返回还能用的一次性加入令牌的哈希（安装命令里的令牌；接入码不经过安装脚本，不算）。
func (q *Queries) ActiveInstallTokens(ctx context.Context, now int64) ([]string, error) {
	return q.strings(ctx, `SELECT token_hash FROM enroll_tokens WHERE expires_at > ? AND used_at IS NULL AND multi_use = 0`, now)
}
