-- 成员最近 90 天登录过的网段（IPv4 /24、IPv6 /48），管理员从新的网段登录时告警（见 docs/audit.md#安全告警）。
CREATE TABLE login_networks (
    user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    network   TEXT NOT NULL,
    last_seen INTEGER NOT NULL,
    PRIMARY KEY (user_id, network)
);
