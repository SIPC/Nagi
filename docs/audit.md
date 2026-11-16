# 审计日志

审计日志记下谁在什么时候、从哪里、通过什么、对什么、做了什么、结果如何。安全事件先写成审计记录，再由检测规则判断要不要告警（见 [security.md](security.md)）。

## 记录范围

| 类别 | 内容 |
| --- | --- |
| `operation` | 会改变状态的接口调用，包括失败和被拒绝的 |
| `read` | 敏感查询：查看环境变量和共享变量的值（`GetProject`、`ListProjects`）、备份密钥、告警配置，读取或下载卷文件和节点文件，导出审计记录 |
| `mcp` | MCP 的工具调用；被拒绝的调用记为 `denied` |
| `session` | 节点终端、容器终端、`exec`、`node_exec` |
| `login` | 登录成功、密码错误、验证码错误、退出、会话过期 |
| `alert` | 节点和面板发出的告警 |
| `security` | 安全事件 |

普通查询（列表、状态、监控、日志流）不记，量太大且不涉及敏感内容。敏感查询登记在 `store.SensitiveReads`：`api` 包的测试列出所有回复里带密钥、变量值或文件内容的查询，没登记的测试失败。界面定时刷新的 `GetProject`、`ListProjects` 每个会话、每个项目每小时只记一条。

不用登录就能调的接口里，初始化以后的 `Setup` 失败、被限速拒绝的登录不记录，免得被拿来刷掉记录。用户名或密码错误按 IP 汇总：每个 IP 每小时第一次失败单独记一条（`login_failed`），之后的只计数，下一个整点写一条汇总（`login_failed_summary`，次数和被尝试最多的 5 个用户名）。密码正确、验证码错误的（`totp_failed`）每次都记。

## 字段

| 字段 | 内容 |
| --- | --- |
| `id`、`time` | 自增编号；毫秒时间 |
| `category`、`action` | 类别；接口方法名或事件名，例如 `Deploy`、`mcp:deploy`、`totp_failed` |
| `actor_type`、`actor_id`、`actor_name` | 操作者：`user`、`token`、`node`、`panel`、`system`。用户名改了以后仍能按 ID 查 |
| `via` | `web`、`mcp`、`cli`、`system`、`node` |
| `token_id`、`client` | 访问令牌；MCP 客户端（`User-Agent`） |
| `session` | 登录会话 ID 的哈希前 12 位，把一次登录里的操作串起来 |
| `ip`、`user_agent` | 来源。经本机 Caddy 转发时按 [panel.md](panel.md#访问) 核对后的地址 |
| `project_id`、`target_type`、`target_id`、`target_name` | 对象：`container`、`node`、`member`、`file`、`tool` |
| `params` | 请求参数（JSON，按下一节脱敏），不超过 4 KB |
| `result`、`error` | `ok`、`failed`、`denied`（没登录、没权限）；错误说明 |
| `duration_ms` | 耗时 |
| `parent_id` | 上级记录：MCP 工具调用触发的接口调用指向那次工具调用 |
| `recording_id` | 会话的录制 |
| `prev_hash`、`hash` | 哈希链：`hash = SHA-256(prev_hash ‖ 记录的规范化 JSON)`，规范化 JSON 包含 `id` |

记录由 `internal/server/audit` 写入。接口调用由 `api` 包的拦截器记（`activity.go`），不走 Connect 的入口（终端、上传下载、`exec`、`node_exec`）和登录由处理函数自己记。

## 参数脱敏

- 请求消息里的敏感字段用 protobuf 的字段选项 `debug_redact = true` 标记，记录时替换为 `"[已隐藏]"`。`audit` 包的测试遍历浏览器接口的所有请求消息（包括嵌套的），字段名包含 `password`、`token`、`secret`、`key`、`code`、`env`、`value`、`content`、`data` 的字符串字段，既没有标记、也没有登记为内容字段（`audit.ContentFields`）或安全字段（`audit.SafeFields`）的，测试失败。另一个测试把所有请求里这类字段填上同一个值，记录的参数里出现这个值就失败。
- 环境变量（`EnvVar`）只记名字，不记值。
- compose、节点文件内容这类内容字段只记 SHA-256 和长度，内容本来就在版本记录里。所有 `bytes` 字段同样只记 SHA-256 和长度。
- 超过 4 KB 的参数截断，包成 `{"truncated": true, "head": …}`。

## 存储与保留

- 审计记录存在面板数据目录下的 `audit.db`（SQLite，WAL），不放进主库，只追加。备用面板每次同步都传整份主库（见 [standby.md](standby.md#同步)），审计数据变多以后会拖慢同步。
- 保留：普通记录 90 天；`security`、`session`、`login`、`mcp` 类 365 天。数据库超过 2 GB 时先删最旧的普通记录。每小时清理一次。
- 升级后面板启动时把主库 `activity` 表的记录复制进 `audit.db`，缺少的字段留空，中断了下次从断点继续。原来的接口 `ListActivity`、`ListProjectEvents`、`ListContainerEvents` 改为查询审计库，返回格式不变。`activity` 表不再写入，下一个版本删除。
- 界面和接口都没有删除审计记录的功能。

## 界面

左侧导航的「审计」（`/audit`），所有角色都能看（内容按下面的权限）。

- **筛选**：筛选框（动作、对象、IP、参数、错误）和时间范围在一行，类别、来源、成员、项目、节点、只看失败放在「筛选」对话框里，生效的条件列在下面，可以单独移除。筛选条件和打开的记录写进地址，可以分享。
- **列表**：每条两行。第一行是动作和对象（「部署 shop / web」「MCP 调用 deploy」）；第二行是灰色的成员 · 来源 · IP · 时间，右边是结果。`security` 类在动作前显示状态标记。有下级记录的可以展开。
- **详情**：点击一条记录，从右侧打开 `Sheet`（手机上从底部）：全部字段、参数 JSON、上级和下级记录。
- **导出**：管理员可以按当前筛选导出 JSON Lines（带哈希）。
- 项目页和容器页的「事件」照旧，只显示这个项目或容器的操作和告警。

## 权限

| 角色 | 能看 |
| --- | --- |
| 管理员 | 全部记录、导出 |
| 操作员、只读成员 | 访问范围内项目的记录；不显示 IP、`user_agent` 和其他成员的会话；只读成员看不到失败详情和参数 |
| 访问令牌 | `query_events` 只返回项目的记录 |

## 接口

| 服务 | 方法 | 用途 |
| --- | --- | --- |
| SettingsService | `ListAuditEvents` | 按条件查询，或按 `parent_id` 查下级记录 |
| SettingsService | `GetAuditEvent` | 详情，带上级和下级记录 |
| SettingsService | `ExportAuditEvents` | 导出 JSON Lines（服务端流），只有管理员能用 |
