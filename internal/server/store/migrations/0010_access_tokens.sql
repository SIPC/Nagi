-- MCP 接入（见 docs/mcp.md）：访问令牌（个人访问令牌和 OAuth 授权发出的令牌）、OAuth 客户端、活动记录的来源。

CREATE TABLE api_tokens (
    id                 INTEGER PRIMARY KEY,
    user_id            INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- 个人令牌是成员起的名字，OAuth 是客户端的名称。
    name               TEXT    NOT NULL,
    -- personal：个人访问令牌；oauth：OAuth 授权。
    source             TEXT    NOT NULL,
    client_id          TEXT    NOT NULL DEFAULT '',
    token_hash         TEXT    NOT NULL UNIQUE,
    -- OAuth 的刷新令牌；prev_refresh_hash 是上一个，被再用时说明刷新令牌泄露了，吊销整个授权。
    refresh_hash       TEXT    NOT NULL DEFAULT '',
    prev_refresh_hash  TEXT    NOT NULL DEFAULT '',
    allow_write        INTEGER NOT NULL DEFAULT 0,
    allow_exec         INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    -- 0 表示不过期。OAuth 的访问令牌 1 小时过期，refresh_expires_at 是整个授权的有效期。
    expires_at         INTEGER NOT NULL DEFAULT 0,
    refresh_expires_at INTEGER NOT NULL DEFAULT 0,
    last_used_at       INTEGER NOT NULL DEFAULT 0,
    last_ip            TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX api_tokens_user ON api_tokens (user_id);
CREATE INDEX api_tokens_refresh ON api_tokens (refresh_hash);
CREATE INDEX api_tokens_prev_refresh ON api_tokens (prev_refresh_hash);

-- 动态注册的 OAuth 客户端。用网址作客户端标识的不存在这里。
CREATE TABLE oauth_clients (
    id            TEXT    PRIMARY KEY,
    name          TEXT    NOT NULL,
    -- JSON 数组。
    redirect_uris TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    -- 第一次被授权的时间，0 表示还没有：24 小时内没被授权的自动删除。
    authorized_at INTEGER NOT NULL DEFAULT 0
);

-- 操作的来源：网页为空，MCP 是 mcp:<令牌名>。
ALTER TABLE activity ADD COLUMN via TEXT NOT NULL DEFAULT '';
