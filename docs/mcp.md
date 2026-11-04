# MCP 接入

让 agent（Claude Code、Claude 网页版和桌面版的连接器、ChatGPT、自己写的 agent）通过 [MCP](https://modelcontextprotocol.io) 直接查看和管理集群：看状态、日志和监控，部署、重启、回滚，在容器里执行命令。agent 用个人访问令牌或者 OAuth 授权登录，权限不超过授权的成员：角色和[访问范围](panel.md#访问范围)照常生效，每次操作都记进活动记录。

能做的事，例如：

- 「orders 的 web 为什么一直重启」：看状态、最近事件、日志和资源占用，给出原因。
- 「把镜像更新到 1.4.2 并部署，失败就回滚」。
- 「哪些节点磁盘快满了」「哪些有状态容器没有备份节点」。
- 「在 api 容器里跑一下数据库迁移」。

## 入口

面板在自己的端口上提供 `/mcp`（MCP 的 Streamable HTTP），和网页同一个地址和证书，不另开端口、不另起进程。不保存会话（Streamable HTTP 的无状态模式）：每个请求单独处理，面板更新、重启后客户端照常接着用；`GET /mcp` 返回 405。被拒绝的请求（401 以外的 4xx）记进面板日志。Go 用官方的 SDK（`github.com/modelcontextprotocol/go-sdk`），代码在 `internal/server/mcp`。

「设置 → 账号安全 → 访问令牌」显示 MCP 地址，生成令牌后给出可以复制的配置。两种接法：

- **能填请求头的客户端**（Claude Code 等）：用个人访问令牌。

  ```bash
  claude mcp add --transport http nagi https://<面板地址>/mcp --header "Authorization: Bearer <令牌>"
  ```

- **只支持 OAuth 的客户端**（Claude 网页版和桌面版的自定义连接器、ChatGPT 的连接器）：只填 `https://<面板地址>/mcp`，客户端会打开面板的授权页。

说明：

- **地址按客户端访问的主机拼**：元数据、授权页、换令牌的地址都是 `https://<客户端访问的主机>`，令牌也只能用于这个地址下的 `/mcp`。绑定了域名时请求经面板自带节点的 Caddy 转发：Caddy 反代面板时保留原来的 Host；面板确认请求是它转发的（来自本机，带着正确的 `X-Nagi-Proxy`）时，也采用 `X-Forwarded-Host`（`auth.RequestHost`），客户端地址同样取它转发来的（`auth.RequestIP`）。
- 云端的客户端从外网访问面板，面板要能从公网访问，并且绑定了域名、有正式证书（[panel.md](panel.md#访问)）。只用 IP 和自签证书的面板只能接本机或内网的客户端，客户端要信任面板的证书。
- **安全入口**：开启后没登录的访问一律 404，MCP 和 OAuth 的地址也一样，要带着入口：MCP 地址是 `https://<面板地址>/<入口>/mcp`，元数据、授权、换令牌的地址都在 `/<入口>` 下（`auth.EntranceMiddleware` 去掉入口再交给面板，`auth.BasePath` 记着前缀）。拿到这个地址的客户端能用，其他人仍然看到 404。带入口的授权页先记下入口的 cookie，再转到 `/oauth/authorize`。入口路径只给管理员看，开了入口时其他成员的令牌页提示向管理员要地址。改了或关了入口，客户端要换地址。
- 备用面板上也有 `/mcp`，只能用查看类的工具（和网页一样，修改返回"这是备用面板"）。OAuth 授权和续期要写数据库，只在主面板上进行；提升备用面板后，已经发出的令牌照样能用。

## 登录

agent 不能用网页的登录会话（cookie 加两步验证）。两种方式发出的都是「访问令牌」，存在同一张表里，权限规则一样，在同一个列表里管理。

### 权限

- 令牌属于授权它的成员，权限是成员的角色和访问范围，再按下面两个选项收窄。成员的角色、范围改了，令牌立即跟着变。
  - **可修改**：默认不勾，只能用查看类的工具。
  - **执行命令**：默认不勾，勾了才能用 `exec`。只读成员没有这一项。
  - **节点终端**：默认不勾，只有管理员的个人访问令牌能勾，要同时勾「执行命令」。勾了才能用 `node_exec`（见下面的[节点命令](#节点命令)）。OAuth 授权没有这一项。成员降为操作员后，令牌上的这一项随之失效。
- 节点文件（见 [node-access.md](node-access.md#权限)）一律不能用令牌。
- 令牌只在 `/mcp` 有效：Connect 接口、终端、上传这些入口不接受令牌，令牌泄露时影响面只有下面列出的工具。

### 个人访问令牌

在「设置 → 账号安全 → 访问令牌」生成：填名称（例如「Claude Code · 笔记本」），选有效期（30 天、90 天、1 年、不过期，默认 90 天）和上面两个选项。令牌是长期有效的凭据，生成前要做高权限确认（15 分钟内不再询问）。令牌只显示一次，形如 `nagi_<43 位 base64url>`，面板只存哈希。

### OAuth

面板自己当授权服务器（OAuth 2.1，按 MCP 规范），不依赖外部服务：

| 地址 | 作用 |
| --- | --- |
| `/.well-known/oauth-protected-resource`、`/.well-known/oauth-authorization-server` | 告诉客户端去哪里授权。没带令牌访问 `/mcp` 时返回 401，`WWW-Authenticate` 里给出前一个地址 |
| `/oauth/register` | 客户端自动注册（动态注册）。也支持用一个网址作为客户端标识（客户端元数据文档），由面板去取它的元数据 |
| `/oauth/authorize` | 授权页。没登录时先走正常登录（包括两步验证和安全入口）；页面写明「<客户端名称> 请求访问 Nagi」和回调地址的域名，成员选有效期和上面两个选项，点「允许」或「拒绝」 |
| `/oauth/token` | 用授权码换令牌，强制 PKCE（S256）。访问令牌 1 小时过期，刷新令牌按授权时选的有效期；每次刷新都换一对新的，旧的刷新令牌再用一次就吊销整个授权（防止被盗用） |
| `/oauth/revoke` | 吊销 |

- 令牌只能用于本面板的 `/mcp`（检查 `resource` 参数，RFC 8707）。
- 回调地址必须是 https 或本机地址，和注册时的严格一致。授权码 5 分钟内有效，只能用一次。
- 回调地址里带 `iss`（RFC 9207），成功和拒绝都带，和元数据里的 `issuer` 完全一样（`auth.IssuerOf`）。ChatGPT 只有服务器声明支持 iss 时才用它客户端元数据里登记的固定回调地址。
- 动态注册是公开接口：注册过的客户端在有人授权之前什么也做不了；最多 100 个，注册后 24 小时没有被授权的自动删除；按来源地址限速。
- 授权页的权限选项默认是最小的（只读、不能执行命令），成员要主动勾选。

### 管理

- 「访问令牌」列表显示名称（OAuth 的是客户端名称）、来源（个人令牌或 OAuth）、权限、创建时间、最后使用的时间和地址、过期时间，可以随时吊销。每人最多 20 个。
- 管理员在「设置 → 成员」从成员行的 ⋯ 打开「访问令牌」，看到这个成员的令牌并吊销，发现泄露时不用等本人处理。看不到令牌本身，也不能替别人生成。
- 删除成员、管理员重置成员密码时，这个成员的令牌全部吊销（和登录会话一样）。
- 数据库：
  - `api_tokens (id, user_id, name, source, client_id, token_hash, refresh_hash, prev_refresh_hash, allow_write, allow_exec, created_at, expires_at, refresh_expires_at, last_used_at, last_ip)`，删除成员时级联删除。最后使用时间 1 分钟内不重复写。
  - `oauth_clients (id, name, redirect_uris, created_at, authorized_at)`：动态注册的客户端。用网址作标识的不存，元数据在内存里缓存 10 分钟。
  - 授权码只放在内存里。
  - 备用面板复制数据库时一起带过去。

## 调用方式

工具不直接调用业务代码，而是在进程内调用面板自己的 Connect 接口：MCP 层验证令牌后，把令牌对应的成员放进请求的 ctx，通过内存里的传输调用同一个 mux（不经过网络，所以不能伪造）。这样现有的拦截器全部照常生效，不用为 MCP 再写一遍：

- 角色检查、访问范围、高权限确认（令牌没有高权限，相关接口一律拒绝）、备用面板只读。
- 活动记录：`activity` 加一列 `via`，网页为空，MCP 写 `mcp:<令牌名>`，活动记录页显示「通过 MCP（令牌名）」。`exec` 和 `node_exec` 不走接口，各自检查权限、单独记一条，详情是命令本身，退出码不为 0 时接在后面。
- 令牌只能调用项目和节点的接口（`auth.TokenAllowed`），成员、设置、令牌自己的接口都不行。
- 没勾「可修改」的令牌在 MCP 层先拦下所有修改类工具，再由接口层兜底；没勾「执行命令」的拦下 `exec`。

## 工具

按任务设计，不把 90 多个接口一一暴露：工具太多 agent 选不准。项目、容器、节点都可以用名称或 ID 指定（项目名可以重名，重名时要求用网络名或 ID）。工具说明是英文的（给模型看），返回 JSON。令牌没有相应权限（或者是备用面板）时，客户端拿到的工具列表里就没有修改类工具、`exec` 和 `node_exec`。

### 查看

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `overview` | 节点在线数、容器运行数、需要处理的事（离线节点、没在运行的容器、可更新的镜像、从未部署的容器数） | `ListNodes`、`ListProjects` |
| `list_projects` | 项目和每个容器的状态、所在节点 | `ListProjects` |
| `get_project` | 修订号、快照间隔和份数上限、内部端口、容器、端口映射、已连接项目、共享变量名（有草稿时另列草稿里的变量名） | `GetProject` |
| `get_container` | 配置（变量只给名称，另列哪些是密钥）、草稿差异、最近 5 个版本、最近 20 条事件、运行状态 | `GetProject`、`ListVersions`、`ListContainerEvents` |
| `list_deployments` | 项目的部署记录：时间、来源、涉及的容器和版本、是否成功 | `ListDeployments` |
| `check_images` | 立即检查项目里的镜像有没有新版本 | `CheckImages` |
| `list_registries` | 项目登录过的私有镜像仓库地址，不返回密码 | `ListRegistries` |
| `list_backup_snapshots` | 备份节点上副本里的快照（截至上次同步） | `ListBackupSnapshots` |
| `list_nodes` | 节点、在线、占用、容器数 | `ListNodes` |
| `query_metrics` | 节点或容器在一段时间内（默认 60 分钟，最长 7 天）的 CPU、内存、磁盘、网络、重启次数：最小、平均、最大、最新和不超过 60 个点 | `QueryNodeMetrics` |
| `get_logs` | 最近的日志，默认 200 行、最多 1000 行，可以只看 stderr、按关键字过滤 | `StreamLogs`（不跟随） |
| `list_snapshots` | 容器的快照：ID、类型、时间、大小，标出能不能用 `restore_snapshot` 恢复、能不能删除，以及项目的份数上限 | `ListSnapshots` |
| `check_routes` | 端口映射的域名检查：是否解析到入口节点、证书状态。用面板自己的检查，不按 agent 给的地址发请求，不能被拿来探测内网 | `CheckDomains` |
| `query_events` | 项目的活动记录，可以按容器、只看失败的筛选，翻页 | `ListProjectEvents` |
| `inspect_node_storage` | 只给管理员（内容涉及节点上所有项目）：Docker 镜像、容器可写层、数据卷、构建缓存的占用和可回收空间，最大的镜像，每个容器的 data、volumes、快照占用。只读，不清理 | `GetNodeStorage`（节点的 `GetStorage`） |

### 日常操作

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `create_project` | 新建项目（不带容器） | `CreateProject` |
| `update_project` | 修改项目名称、网络名、快照间隔、份数上限、内部端口，立即生效。`expected_revision` 是 `get_project` 返回的项目修订号 | `UpdateProjectSettings` |
| `update_shared_env` | 设置、删除共享变量，或标为密钥，存为草稿，返回引用了改动变量的容器。用 `deploy` 的 `apply_shared_env` 生效 | `SaveSharedEnv` |
| `discard_shared_env` | 丢弃共享变量的草稿 | `DiscardSharedEnv` |
| `connect_project`、`disconnect_project` | 连接、断开两个项目，立即生效 | `ConnectProject`、`DisconnectProject` |
| `login_registry`、`logout_registry` | 登录、退出项目的私有镜像仓库。密码存在面板上，不返回 | `LoginRegistry`、`LogoutRegistry` |
| `add_container` | 用镜像或 compose 添加容器，节点默认和网页一样选项目里容器最多的在线节点；默认不部署，要部署时传 `deploy` 或再调用 `deploy`。镜像表单的端口就是发布到节点上的端口，项目里的容器互相访问不需要 | `AddContainer`、`Deploy` |
| `update_container` | 修改配置存为草稿，覆盖网页上容器配置的所有项：镜像、启动命令、端口、持久化目录、工作目录、资源限制、健康检查，或整份 compose；容器变量（设置、删除、标为密钥）、容器组的服务级环境变量；git 来源、保留路径、有无状态、部署依赖。返回改了哪些，不部署。带 `expected_revision`（`get_container` 返回的修订号）时，期间被别人改过就拒绝。`add_container` 也接受同样的配置项 | `SaveDraft` |
| `set_display_name` | 修改容器的显示名称 | `SetContainerDisplayName` |
| `discard_draft` | 丢弃草稿 | `DiscardDraft` |
| `deploy` | 部署一个或多个容器（有草稿的用草稿）。过程中用进度通知推送部署事件，结束后返回结果和失败时的最后 50 行输出；超过 10 分钟返回「仍在进行」，之后用 `get_container` 查。`expected_revisions` 同上；`wait_healthy_seconds`（最多 300）在部署成功后等容器运行、有健康检查的变成 healthy，不健康或停了算不通过，只回报，不自动回滚。`apply_shared_env` 先让共享变量的草稿生效，不指定容器时只生效、不部署 | `Deploy`、`GetProject` |
| `restore_snapshot` | 从定时或手动快照恢复数据。不带 `confirm` 只返回预览（会发生什么、数据会怎样）；`confirm` 原样填快照 ID 才执行。执行前先给当前数据拍一份保护快照，拍失败就不恢复；保护快照会让要恢复的快照因份数上限被删掉时，直接拒绝。恢复时停止容器、替换 data/ 和 volumes/，配置不变 | `ListSnapshots`、`CreateSnapshot`、`RestoreSnapshot` |
| `control_container` | 启动、停止、重启 | `ControlContainer` |
| `rollback` | 回到某个版本，只回配置和文件。带 `restore_data` 时同时把数据恢复到这个版本被替换之前，要先预览、`confirm` 原样填版本号才执行 | `Rollback` |
| `create_snapshot` | 立即做一次快照 | `CreateSnapshot` |
| `delete_snapshot` | 删除定时、手动或部署前快照，版本文件不能删。和恢复一样分两步：不带 `confirm` 只返回预览，`confirm` 原样填快照 ID 才删除 | `ListSnapshots`、`DeleteSnapshot` |
| `save_route` | 新建或修改端口映射（域名、HTTPS、TCP/UDP、跨节点入口） | `SaveRoute` |
| `delete_route` | 删除端口映射，立即生效。不带 `confirm` 只返回预览，`confirm` 原样填映射名才删除 | `DeleteRoute` |
| `retry_cert` | 让入口节点立即重新申请域名的证书。不指定节点时，对证书还没拿到的入口节点都重试 | `CheckDomains`、`RetryDomainCert` |
| `sync_backup` | 立即同步到备份节点，同步完才返回 | `SyncBackup` |
| `exec` | 在容器里执行一条命令，不分配终端、没有输入，默认 30 秒、最长 120 秒超时，输出最多 64 KB（超出时保留开头和结尾）。权限和终端一样，只读成员不能用；令牌要勾「执行命令」 | 节点的 `Exec`（和 `/ws/exec` 同一段代码） |

### 不可逆或会中断服务的操作

都分两步：不带 `confirm` 只返回预览（会发生什么、会丢什么），agent 给用户看过、用户同意后，再把 `confirm` 原样填成说明里写的值调用一次才执行。都标为 `destructiveHint`。

| 工具 | 内容 | `confirm` 填 | 用到的接口 |
| --- | --- | --- | --- |
| `delete_container` | 停止并删除容器，连同版本和端口映射。容器目录默认保留，`delete_dir` 时一起删除。预览列出相关的端口映射和依赖它的容器 | 容器名 | `DeleteContainer` |
| `delete_project` | 删除项目和全部容器。容器目录默认保留，`delete_dirs` 时一起删除 | 项目名 | `DeleteProject` |
| `restore_deployment` | 回到某次部署时的状态：版本不一样的容器回滚到那次的版本，一起部署，不恢复数据。预览列出每个容器的版本变化 | 部署记录 ID | `ListDeployments`、`RestoreDeployment` |
| `set_backup_node` | 设置备份节点（立即生效，不用确认）；`node` 留空时取消，取消要先预览，`delete_replica` 时同时删除备份节点上的副本 | 容器名（取消时） | `SetBackupNode` |
| `restore_from_backup` | 原节点离线时，在备份节点上用副本里的快照把容器恢复出来，之后写入的数据会丢。预览写明会用哪一份快照 | 容器名 | `ListBackupSnapshots`、`RestoreFromBackup` |
| `migrate_container` | 把容器迁移到另一个节点，不丢数据，最后一次同步时短暂停止。两个节点都要在线 | 目标节点名 | `MigrateContainer` |
| `redeploy_elsewhere` | 原节点离线、又没有副本时的最后手段：在另一个节点上重新部署，数据为空、旧版本不能回滚 | 目标节点名 | `RedeployElsewhere` |
| `set_failover` | 设置顶替节点和见证人。有状态的容器默认等 `confirm_takeover` | 顶替节点名 | `SetFailover` |
| `clear_failover` | 取消顶替 | 容器名 | `ClearFailover` |
| `confirm_takeover` | 有状态容器的节点失联、热备在等确认时，让热备用最近同步的数据顶替 | 容器名 | `ConfirmTakeover` |

`change_witness`（更换见证人，顶替照常进行）不中断服务，不用确认。`get_container` 返回备份节点的同步状态和顶替的状态（在哪个节点上运行、最近一次同步、是否在等确认）。

### 文件

读写容器卷里的文件，和网页的文件管理走同一条路：经 mesh 传给容器所在的节点，节点把操作限制在卷里。归在「执行命令」权限下（能执行命令本来就能读写容器里的文件）：只读成员不能用，令牌要勾「执行命令」，访问范围外的项目不能用，备用面板不能写。

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `list_files` | 不带 `volume` 时列出容器的卷（`.data` 是容器的 data/ 目录，部署时会被替换，保留的路径除外）；带上时列出卷里的一个目录，带 `snapshot_id` 时列出快照里的 | `ListVolumes`、`ListVolumeFiles` |
| `read_file` | 读一个文本文件，最多 256 KB，返回内容和 SHA-256。二进制文件和目录拒绝 | 节点的 `VolumeDownload` |
| `write_file` | 新建或整个覆盖一个文本文件，最多 1 MB。文件已经在时必须带 `expected_sha256`（`read_file` 返回的），期间被改过就拒绝；已有的文件是二进制或超过 1 MB 时不能覆盖 | 节点的 `VolumeUpload` |
| `create_dir` | 在卷里新建目录（连同上级目录） | `CreateVolumeDir` |
| `restore_file` | 把定时、手动或部署前快照里的一个文件或目录恢复到原位，不停容器：快照里有的文件覆盖当前的，别的不动。不带 `confirm` 只返回预览，`confirm` 原样填快照 ID 才执行 | `ListSnapshots`、`RestoreVolumeFile` |
| `delete_file` | 删除卷里的一个文件或目录（连同里面的内容）。不带 `confirm` 只返回预览，`confirm` 原样填路径才删除 | `DeleteVolumeFile` |

不提供改名。读写都记进活动记录（`DownloadVolumeFile`、`UploadVolumeFile`、`CreateVolumeDir`、`RestoreVolumeFile`、`DeleteVolumeFile`），带着卷和路径。`create_dir`、`restore_file`、`delete_file` 会修改卷，令牌还要勾「可修改」。

### 节点命令

`node_exec` 以 root 身份在节点上执行一条命令，等它结束，返回输出（最多 64 KB）和退出码。默认 30 秒、最长 120 秒超时，到时结束整个进程组。这等于把整台机器交给 agent，所以比网页的节点终端还多几道限制：

- 只有管理员能用，令牌要勾「节点终端」（只有管理员的个人访问令牌能勾，生成前要做高权限确认）。没勾的令牌看不到这个工具。
- 节点在本地关闭了节点访问（`nagi node access disable`）时拒绝，面板没法替节点打开。备用面板上没有这个工具。
- 每次执行都记进活动记录（`NodeExec`，详情是命令和退出码），并向告警配置里的渠道发一条通知（不带命令内容）；节点也记一条本地日志，带面板指纹、用户名和令牌名。
- shell 和环境变量和节点终端一样（见 [node-access.md](node-access.md#终端)）：root 的登录 shell，`sh -lc` 执行，不分配终端，没有输入。节点跑在容器里时，命令进的是这个容器，返回里会注明。
- 和 `exec` 共用每分钟 10 次的限额。节点要升级到支持 `NodeExec` 的版本，旧节点返回「节点版本过旧」。文件内容和日志一样当作不可信数据。

工具都带 MCP 的工具标注（`readOnlyHint`、`destructiveHint`、`openWorldHint`、`idempotentHint`），`_meta.securitySchemes` 里写需要的 OAuth 范围（查看类 `nagi:read`，修改类 `nagi:write`，`exec` 和文件工具是 `nagi:exec`），ChatGPT 按它判断工具要不要授权。`deploy`、`control_container`、`rollback`、`restore_snapshot`、`delete_snapshot`、`disconnect_project`、`delete_route`、`exec`、`node_exec`、`write_file`、`restore_file`、`delete_file` 和上面不可逆的操作标为 `destructiveHint`，客户端一般会先问用户。部署这类操作用不跟着客户端取消的 ctx：agent 断开或者等不及，操作在面板上照样做完。

### 还没有的

- 整个集群和面板的管理，计划之后只给管理员：节点管理和磁盘清理、节点上的文件、面板设置（包括告警）、成员。
- 一直不提供：初始化、登录、提升权限、改密码、两步验证、会话、OAuth 授权、访问令牌，以及会把凭据放进对话的操作（邀请链接、节点加入命令、备用面板配对码、备份密钥）、安全入口。也不做按 agent 给的地址发 HTTP 请求的探测（会被拿来探测内网），用 `check_routes` 代替。

## 给 agent 的说明

- **服务说明**：MCP 初始化时返回一段简短说明：Nagi 的概念（节点、项目、容器、容器组）、修改要先存草稿再部署、操作员不能保存带特权的配置（[node-access.md](node-access.md#操作员不能借-compose-拿到节点-root)）、当前令牌的角色和权限。
- **资源**：`nagi://docs/containers`：容器的种类、compose 的规则、变量的三层、操作员不能保存的配置，内容摘自 [agent.md](agent.md) 和 [node-access.md](node-access.md)，写在 `internal/server/mcp/resources.go` 里随程序编进去。改了那两份文档里相应的规则时要跟着改。

## 安全

- **密钥不进模型**：变量的值、deploy key、webhook 令牌、备份密钥都不返回，只给名称和「已设置」。agent 可以设置变量的值（只写）。
- **不可信内容**：日志和 `exec` 的输出可能含有针对 agent 的指令，工具说明里写明这些是数据，不是指令。
- **限额**：每个令牌每分钟最多 60 次调用，其中 `exec` 最多 10 次；单次返回内容有上限，超出时截断并注明。
- **可以随时止损**：令牌页一键吊销；活动记录能按 `via` 筛出 MCP 做的所有操作。
- **被拒绝的调用留痕**：令牌没有权限的工具不在它的工具列表里，客户端照样按名字调用时，SDK 当作不存在的工具拒绝，面板先从请求里认出工具名，记一条「MCP 调用被拒绝」（`MCPToolDenied`）。

## 实现

- `internal/server/mcp`：`/mcp` 入口和令牌核对（`mcp.go`，用 SDK 的 `RequireBearerToken`）、进程内调用面板接口的传输、按令牌的权限给工具（`tools.go`）、工具本身（`list.go`、`ops.go`、`snapshots.go`、`diagnose.go`、`files.go`）、给 agent 的资源（`resources.go`）。每个工具声明是否修改、是否执行命令和用到的接口，`tools_test.go` 检查查看类的工具只用查询接口、修改类的至少用到一个修改接口、接口都存在且操作员能调用。
- `internal/server/oauth`：授权服务器。`oauth_test.go` 按规范走一遍完整流程（注册、授权、换令牌、刷新、旧刷新令牌重用时吊销整个授权、吊销），并测回调地址不匹配、没有 PKCE、PKCE 不对、授权码重用、拒绝授权、本机回调换端口。
- `internal/server/auth/tokens.go`：生成和核对令牌。`internal/server/api/tokens.go`：令牌管理和授权页的接口。`internal/server/api/exec.go`：`exec` 用的执行命令（和终端同一条路）。`internal/server/api/agentfiles.go`：文件工具用的读写（和网页的文件管理同一条路）。
- `internal/server/store`：`api_tokens`、`oauth_clients` 表，`activity.via` 列（迁移 0010）。查询手写在 `tokens.go`，不放进 `queries.sql`。
- 界面：账号安全里的「访问令牌」（`pages/tokens.tsx`），成员行的 ⋯ 里的「访问令牌」（管理员查看和吊销），授权页 `/oauth/authorize`（没登录时记下地址，登录后回来），活动记录显示来源。
