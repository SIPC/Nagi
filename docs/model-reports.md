# 告警模型综合报告 / Alert model reports

## 安全边界 / Safety

默认关闭，只有管理员在设置中明确启用后才能手动生成。接口为用户自己的 OpenAI 兼容 **完整 HTTPS chat/completions 地址**、模型名和 API 密钥；不提供默认供应商、不自动测试连接。沿用面板主密钥 AES-GCM 加密配置，密钥只写不读，留空保留原值。不得使用 URL 用户信息、查询串或片段承载凭据。外发使用现有 safehttp：默认仅公网，拒绝重定向、DNS 重绑定、环境代理和元数据地址。勾选「允许内网地址」后也可连接内网、CGNAT 和面板本机网卡上的地址（例如与面板同机的中转），本机回环、链路本地和云元数据地址始终不允许，规则与 webhook 渠道相同。连接失败只记录稳定类别（address_blocked、dns_failed、connect_failed、tls_failed、network_timeout、request_failed）。

Disabled by default. An administrator must explicitly enable their own OpenAI-compatible full HTTPS chat/completions URL, model and write-only API key. No default provider or automatic connection test. Credentials use the panel's existing master-key encryption. Public destinations only by default, no redirects or environment proxy. "Allow private addresses" also permits LAN, CGNAT and the panel machine's own interface addresses (e.g. a relay on the same server); loopback, link-local and cloud metadata stay blocked, as for webhook channels. Connection failures record only a stable category.

只外发白名单告警摘要（规则种类、状态、有效性、时间和报告内别名）及数值统计，再经现有统一脱敏。不发送原始日志、告警自由文本标题/正文、人工结论、IP、域名、项目/容器名称、凭据或配置。模型结果也是敏感、未经事实核验的建议；只以纯文本显示，不执行操作、不封 IP。不发送工具、不保存 reasoning_content 或模型私密推理。脱敏并非任意文本绝不泄密的保证，因此源材料首先按白名单选字段。

Only allowlisted alert fields and numeric summaries, with per-report aliases, pass through existing redaction before sending. No raw logs, free-text alert titles/messages, human conclusions, IPs, domains, project/container names, credentials or configuration. Output is unverified advice, rendered as plain text. No tools, actions, IP blocking or private reasoning storage.

## 保守暂定规则（待用户确认） / Provisional policy

- 仅手动：一次合并最近 30 分钟的告警变更及当前未恢复告警；不实现自动批次触发，避免在用户确认前持续产生费用。旧的当前告警明确标识过期/未知，不当作近期新发生。
- 同一面板串行，15 分钟冷却；滚动 24 小时最多 12 次，失败/超时也计数；限额随开始记录原子保存，重启、清理报告或开关切换不重置。多台面板/故障提升暂不承诺全组配额。
- 最多 60 秒模型请求；统计收集另设 10 秒总预算。请求取消则取消外发并记录取消；无自动重试，不回放崩溃前请求，超过 2 分钟未完成显示 interrupted，不冒称成功或已知外部计费结果。
- 输入 JSON 最多 16 KiB，模型最终文字最多 12 KiB，HTTP 响应最多 64 KiB，max_tokens=2048。材料过大拒绝生成，不静默截断证据；非 stop 结束、空文字、损坏 JSON 或超限响应记失败。供应商是否真正遵循 token 上限由对方实现决定。
- 报告和生成记录存在面板所在机器顶层 logs/logs.db 的 task 类别（object=model-reports）；目标保留 7 天，读取/生成时过期清理，空闲期间不承诺准点删除；容量可提前淘汰。正文、索引、WAL 都计现有共享 5～10 GiB，清理留摘要，不另开无限存储。
- 同统一跨节点日志查询，本版只给管理员（报告跨项目/主机，不向项目成员或仅有单项告警规则权限的成员开放）。备用只允许读本机已有报告，不生成；本版不新增主备报告正文复制承诺。

Manual only; 30-minute window plus outstanding alerts; serialized per panel; 15-minute cooldown; at most 12 attempts per rolling 24 hours including failures. Durable quota survives restarts and history cleanup. Model timeout 60 seconds, collection budget 10 seconds, no automatic retries. Input 16 KiB, final text 12 KiB, provider response 64 KiB and max_tokens=2048. Seven-day target retention, lazily expired on reads/generation and subject to earlier shared-capacity eviction. Administrator only, standby read-only. Cross-panel quota and report replication are not guaranteed.

每次保留触发原因、操作者、时间段、缺数据、实际外发材料与 SHA-256、请求模型和模板版本、起止/失败/超时/取消及最终报告。外部错误只记稳定类别/HTTP 状态码，不记录 URL、响应正文、Authorization 或私密推理。开始记录不能落盘则不外发；结果落盘失败明确返回失败，不能保证断电/磁盘损坏时保住未落盘结果。没有“采纳”按钮，采纳与否未跟踪。

Each attempt retains its trigger, actor, time window, missing evidence, actual redacted material and digest, requested model/template version, start/end/outcome and final text. Provider errors retain only safe categories/status codes. Start persistence is required before sending. Result persistence failures are surfaced; no claim of crash-proof delivery or adoption tracking.

## 本版材料覆盖 / Evidence coverage

节点材料取已登记节点的告警账本及兼容的当前内存告警，面板告警取近 30 分钟统一日志中的 firing/resolved 元信息（最多查询 1000 条，达到上限注明缺失）。旧日志 key 已被统一脱敏时规则归 other，不从正文猜测。最多 60 条摘要、8 个相关节点，超过则拒绝本次收集；只查询这些节点的 node:cpu/mem/disk/net_rx/net_tx，每分钟聚合点的数量、最小/最大/均值与首尾时间，CPU/内存/磁盘单位为百分比、网络为字节/秒。没有样本、mesh 不通、历史可能淘汰均明确标注。本版不发送路由、容器、按来源 IP 统计或原始日志，不能靠这些材料确认 DDoS。材料内 N/A/M 是本次报告的节点/告警/统计引用，不对外发送映射表。

Node evidence comes from registered-node ledgers and legacy active-alert memory. Panel lifecycle metadata comes from up to 1000 recent unified alert events; an archive-redacted key becomes other, never reconstructed from prose. More than 60 alerts or 8 related nodes rejects collection. Only those nodes' CPU/memory/disk percentages and network bytes/second are summarized from minute aggregates (count/min/max/mean/first/last). Missing samples, unreachable mesh and possibly pruned history are explicit. Route/container/source-IP statistics are not included in this version and the material cannot establish DDoS. Per-report N/A/M references have no outbound identity mapping.
