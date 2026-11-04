-- 访问令牌可以在节点上执行命令（MCP 的 node_exec，见 docs/mcp.md#节点命令）：只给管理员的个人访问令牌，创建时单独勾选。
ALTER TABLE api_tokens ADD COLUMN allow_node_exec INTEGER NOT NULL DEFAULT 0;
