-- 访问令牌的网段限制和新网段告警（见 docs/mcp.md#网段）。
-- networks：允许使用的 IP 或网段，一行一个，空表示不限制。
-- seen_networks：用过的网段（IPv4 /24、IPv6 /48），一行一个，最多记 20 个；用于「第一次从新网段使用」的告警。
-- last_client：最近一次使用的客户端（User-Agent）。
ALTER TABLE api_tokens ADD COLUMN networks TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN seen_networks TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN last_client TEXT NOT NULL DEFAULT '';
