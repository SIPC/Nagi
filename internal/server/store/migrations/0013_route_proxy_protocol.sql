-- 端口映射转发给容器时加 PROXY protocol 头（见 docs/exposure.md#tcp--udp-服务）。
ALTER TABLE routes ADD COLUMN proxy_protocol INTEGER NOT NULL DEFAULT 0;
