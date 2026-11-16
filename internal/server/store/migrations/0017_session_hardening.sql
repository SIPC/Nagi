-- 会话加固（见 docs/panel.md#会话）：固定的会话 ID、令牌轮换、会话密钥、设备和网络。
ALTER TABLE sessions ADD COLUMN id TEXT NOT NULL DEFAULT '';
UPDATE sessions SET id = substr(token_hash, 1, 32);
CREATE UNIQUE INDEX sessions_id ON sessions (id);
-- 当前令牌的发放时间：每 15 分钟轮换一次。
ALTER TABLE sessions ADD COLUMN rotated_at INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET rotated_at = created_at;
-- 等待浏览器签名的挑战和发出时间。
ALTER TABLE sessions ADD COLUMN challenge TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN challenge_at INTEGER NOT NULL DEFAULT 0;
-- 会话密钥的公钥（ECDSA P-256，SPKI）。为空表示还没登记。
ALTER TABLE sessions ADD COLUMN public_key BLOB NOT NULL DEFAULT x'';
-- 允许（重新）登记会话密钥的截止时间：登录后和重新验证后的一小段时间。
ALTER TABLE sessions ADD COLUMN key_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET last_seen = created_at, last_ip = ip;
ALTER TABLE sessions ADD COLUMN tls_fp TEXT NOT NULL DEFAULT '';
-- 网段记录（JSON）：每种地址当前和上一个网段、离开上一个的时间，以及验证过的网段。
ALTER TABLE sessions ADD COLUMN nets TEXT NOT NULL DEFAULT '';
-- 高权限确认时的 IP。
ALTER TABLE sessions ADD COLUMN elevated_ip TEXT NOT NULL DEFAULT '';
-- 要求重新验证的原因，为空表示不用；连续失败的次数。
ALTER TABLE sessions ADD COLUMN reverify TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN reverify_failures INTEGER NOT NULL DEFAULT 0;

-- 会话的其他令牌：已发出、浏览器还没用过的新令牌（pending），和换下来的旧令牌（retired，用来发现重用）。
CREATE TABLE session_tokens (
    token_hash TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    state      TEXT NOT NULL,              -- pending、retired
    created_at INTEGER NOT NULL,
    retired_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX session_tokens_session ON session_tokens (session_id);
