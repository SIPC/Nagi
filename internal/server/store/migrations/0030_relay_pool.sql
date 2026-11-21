-- 中转池（见 docs/mesh.md#中转池）：管理员在节点页打开的节点，同意为别的节点承担性能中转。默认不在池里。
-- 和节点自己的「中转其他节点」不同：那个是故障兜底时能不能借它转，这个是直连可用时能不能为了更快经它转。
ALTER TABLE nodes ADD COLUMN relay_pool INTEGER NOT NULL DEFAULT 0;
