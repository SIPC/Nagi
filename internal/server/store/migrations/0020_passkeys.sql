-- 通行密钥（Passkey，见 docs/panel.md#通行密钥）。rp_id 是添加时面板绑定的域名，换了域名以后旧的用不了。
CREATE TABLE passkeys (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL,
    credential_id BLOB    NOT NULL UNIQUE,
    public_key    BLOB    NOT NULL, -- COSE 格式
    sign_count    INTEGER NOT NULL DEFAULT 0,
    name          TEXT    NOT NULL,
    rp_id         TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX passkeys_user ON passkeys (user_id);
CREATE TRIGGER passkeys_user_delete AFTER DELETE ON users BEGIN
    DELETE FROM passkeys WHERE user_id = OLD.id;
END;
