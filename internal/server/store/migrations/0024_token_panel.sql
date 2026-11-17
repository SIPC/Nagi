-- 访问令牌可以管理面板（MCP 的面板设置、告警渠道、审计、面板更新等，见 docs/mcp.md#管理面板）：只给管理员，生成或授权时单独勾选。
ALTER TABLE api_tokens ADD COLUMN allow_panel INTEGER NOT NULL DEFAULT 0;
