# MCP 接入

让 agent（Claude Code、Claude 网页版和桌面版的连接器、ChatGPT、自己写的 agent）通过 [MCP](https://modelcontextprotocol.io) 直接查看和管理集群：看状态、日志和监控，部署、重启、回滚，在容器里执行命令。agent 用个人访问令牌或者 OAuth 授权登录，权限不超过授权的成员：角色和[访问范围](panel.md#访问范围)照常生效，每次操作都记进[审计日志](audit.md)。

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

- 令牌属于授权它的成员，权限是成员的角色和访问范围，再按下面几个选项收窄。成员的角色、范围改了，令牌立即跟着变。
  - **可修改**：默认不勾，只能用查看类的工具。
  - **执行命令**：默认不勾，勾了才能用 `exec`。只读成员没有这一项。
  - **节点终端**：默认不勾，只有管理员能勾（个人访问令牌和 OAuth 授权都可以），要同时勾「执行命令」。勾了才能用 `node_exec`（见下面的[节点命令](#节点命令)）和节点文件、导入现有目录、隔离节点、副本的存放位置这些网页上要先做高权限确认的操作（见[高权限确认](#高权限确认)），OAuth 范围是 `nagi:node`。成员降为操作员后，令牌上的这一项随之失效。
  - **管理面板**：默认不勾，只有管理员能勾（个人访问令牌和 OAuth 授权都可以）。勾了才有[管理面板](#管理面板)的工具：面板设置、告警渠道、活动和审计、面板更新、备用面板状态、成员（只读）；修改的还要勾「可修改」。每次使用都发告警。OAuth 范围是 `nagi:admin`。成员降为操作员后随之失效。
- 节点镜像（见下面的[节点镜像](#节点镜像)）只给管理员的令牌：查看不用额外权限，删除、清理、拉取要勾「可修改」。
- 直连下载的链接和命令（`get_download_link`、`get_node_download_link`，见下面的[文件](#文件)）只给管理员的令牌：卷文件要勾「执行命令」（和 `read_file` 一样），节点文件要勾「节点终端」（和 `read_node_file` 一样）。
- 令牌只在 `/mcp` 有效：Connect 接口、终端、上传这些入口不接受令牌，令牌泄露时影响面只有下面列出的工具。

### 个人访问令牌

在「设置 → 账号安全 → 访问令牌」生成：填名称（例如「Claude Code · 笔记本」），选有效期（30 天、90 天、1 年、不过期，默认 90 天）和上面的选项，可以填写允许使用的地址（见[网段](#网段)）。令牌是长期有效的凭据，生成前要做高权限确认（15 分钟内不再询问）。令牌只显示一次，形如 `nagi_<43 位 base64url>`，面板只存哈希。

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
- 动态注册是公开接口：注册过的客户端在有人授权之前什么也做不了；最多 100 个，注册后 24 小时没有被授权的自动删除；按来源地址限速（每小时 10 个，IPv6 按 /64 合并，最多记 1 万个来源，满了以后新来源暂时不能注册）。
- 授权页的权限选项默认是最小的（只读、不能执行命令、没有节点终端、不能管理面板），成员要主动勾选。「节点终端」「管理面板」只有管理员看得到。

### 管理

- 「访问令牌」列表显示名称（OAuth 的是客户端名称）、来源（个人令牌或 OAuth）、权限、允许的地址、创建时间、最后使用的时间、地址和客户端、过期时间，可以随时吊销。每人最多 20 个。
- 管理员在「设置 → 成员」从成员行的 ⋯ 打开「访问令牌」，看到这个成员的令牌并吊销，发现泄露时不用等本人处理。看不到令牌本身，也不能替别人生成。
- 删除成员、管理员重置成员密码时，这个成员的令牌全部吊销（和登录会话一样）。
- 数据库：
  - `api_tokens (id, user_id, name, source, client_id, token_hash, refresh_hash, prev_refresh_hash, allow_write, allow_exec, allow_node_exec, allow_panel, created_at, expires_at, refresh_expires_at, last_used_at, last_ip, networks, seen_networks, last_client)`，删除成员时级联删除。最后使用时间 1 分钟内不重复写（地址或客户端变了照写）。
  - `oauth_clients (id, name, redirect_uris, created_at, authorized_at)`：动态注册的客户端。用网址作标识的不存，元数据在内存里缓存 10 分钟。
  - 授权码只放在内存里。
  - 备用面板复制数据库时一起带过去。

### 网段

令牌存在 MCP 客户端的配置文件里，被偷走后别人能在任何地方用它（`auth/tokennet.go`）。

- **允许的地址**：生成个人访问令牌时可选填写 IP 或网段（`CreateAccessTokenRequest.networks`，最多 50 项，存在 `api_tokens.networks`）。从其他地址使用时当作令牌无效（客户端看到 401），记入审计（`token_network_denied`，类别 security，参数 `rule` 是 `token`）并告警「访问令牌 `<名称>` 从允许范围以外的地址使用，已拒绝」。OAuth 授权的令牌不能填。设了[管理员登录地址](panel.md#管理员登录地址)时，管理员的所有令牌（包括 OAuth 授权的）还要在那个范围内，否则按同样的方式拒绝（`rule` 是 `admin`）。
- **新网段告警**：每个令牌记下用过的网段（IPv4 /24、IPv6 /48，最多 20 个，`seen_networks`；最早用过的 10 个一直留着，满了替换之后最早的那个，偷到令牌的人轮流换网段挤不掉本人一开始常用的）。勾了「执行命令」「节点终端」或「管理面板」的令牌第一次从新网段使用时告警「访问令牌 `<名称>` 第一次从网段 `<网段>` 使用」，带来源和客户端，记入审计（`token_new_network`）；令牌第一次使用不算。每个令牌每小时最多告警一次，这一小时里其余的新网段只记审计，下一条告警里带上个数，轮流换网段刷不爆告警。只读和只能修改的令牌只记录不告警。
- **客户端**：记下最近一次使用的客户端（User-Agent，`last_client`），列表里悬停最后使用时间可以看到。

## 调用方式

工具不直接调用业务代码，而是在进程内调用面板自己的 Connect 接口：MCP 层验证令牌后，把令牌对应的成员放进请求的 ctx，通过内存里的传输调用同一个 mux（不经过网络，所以不能伪造）。这样现有的拦截器全部照常生效，不用为 MCP 再写一遍：

- 角色检查、访问范围、高权限确认（令牌的规则见[高权限确认](#高权限确认)）、备用面板只读。
- 审计记录：每次工具调用都记一条（包括查询），它触发的接口调用挂在下面（见 [audit.md](audit.md#mcp-调用)）。来源 `via` 是 `mcp`，带上令牌 ID 和客户端，审计页显示「MCP（令牌名）」。`exec` 和 `node_exec` 不走接口，各自检查权限、单独记一条（类别 `session`），参数里是命令本身，退出码不为 0 时接在后面。
- 令牌能调用的接口由 `auth.TokenAllowed` 决定：项目和节点的接口（没勾「可修改」时只能查询，高权限确认的那些按下表）；设置和账号的接口只有[管理面板](#管理面板)列出的几个（要勾「管理面板」）和自己的项目页排序，成员的修改、令牌、密码、两步验证、通行密钥、会话这些一律不行（`auth.tokenAccount`，`tools_test.go` 的 `TestTokenNeverReaches` 逐个检查设置、账号、初始化的全部接口）。
- 没勾「可修改」的令牌在 MCP 层先拦下所有修改类工具，再由接口层兜底；没勾「执行命令」的拦下 `exec` 和文件工具；没勾「节点终端」的拦下节点命令和节点文件；没勾「管理面板」的拦下管理面板的工具。

### 高权限确认

网页上有些操作要先再验证一次身份（高权限确认，见 [node-access.md](node-access.md#权限)）。令牌做不了交互式的确认，所以一张表（`auth.tokenElevated`）、一个函数（`auth.User.ElevatedFor`）说清楚：表里的接口，管理员的令牌勾了要求的选项就算确认过，MCP 工具每次修改前再让用户确认预览；不在表里的，令牌一律不算确认过。网页会话的判断不变（`Elevated`：15 分钟内做过确认）。`api` 的拦截器只调用这一个函数（`needsElevation`），`api/token_elevation_test.go` 逐个检查 `elevationRequired` 里的接口。

| 接口 | 令牌要勾 | 原因 |
| --- | --- | --- |
| 节点文件的查看：`ListNodeDir`、`ReadNodeFile`、`ListNodeFileBackups`、`ListNodeDirSizes`，以及 `InspectComposeDir` | 节点终端 | 读的是整台机器的文件，等于节点的 root |
| 节点文件的修改：`WriteNodeFile`、`DeleteNodeFile`、`CreateNodeDir`、`RestoreNodeFileBackup`、`RenameNodeFile`、`MoveNodeFiles`、`CopyNodeFiles`、`CompressNodeFiles`、`ExtractNodeFile`；`ImportComposeDir`、`IsolateNode`、`UnisolateNode`、`ConfirmPendingHA` | 节点终端、可修改 | 以 root 改节点上的东西，或者让别的节点运行拼回的配置 |
| `SetNodeReplicaDir`、`SetBackupReplicaDir`（`SetBackupNodes` 指定存放位置时同样） | 节点终端、可修改 | 让节点以 root 在任意位置建目录、写数据 |
| `DeleteNodeImages`、`PruneNodeImages` | 可修改 | 节点拒绝删有容器在用、Nagi 需要的镜像，影响和部署同一级 |
| 设置和账号的：`CreateAccessToken`、`BeginTOTP`、`BeginPasskeyRegistration`、`SetAdminNetworks`、`GetRecording`、`TestAuditSink` | 令牌一律不行 | 会给自己留登录凭据、把账号主人锁在外面，或者泄露录制和外送的凭据 |

「节点终端」只能在做过高权限确认之后勾选（生成令牌前、在授权页点允许前），所以这些令牌已经确认过身份；令牌只在 `/mcp` 有效，到接口的令牌请求都来自 MCP 工具。

### 确认码

要确认的工具分两步：不带 `confirm` 只返回预览和确认码，用户同意后用同样的参数再调用一次，`confirm` 填这个确认码才执行。确认码（`confirm.go`）是 HMAC-SHA256 取前 6 字节，混入了：

- 工具名和这次操作的参数：预览之后改了任何一项都对不上，只会再返回一次预览；
- 面板进程启动时生成的随机密钥（不落盘）：调用方按参数自己算不出来，确认码只能来自预览，agent 跳过预览直接执行会被拦下；面板重启以后作废；
- 成员和令牌 ID：别的令牌拿到的确认码用不了；
- 10 分钟一段的时间段：当前和上一个时间段的都认，所以预览后至少 10 分钟、至多 20 分钟内有效，过期后再预览一次。

确认码只保证执行的就是预览过的操作，不能证明用户同意过：用户同意靠 agent 的说明和客户端对 `destructiveHint` 工具的确认提示，这不是权限边界。填名称的确认（例如删除容器填容器名）不是确认码，不受这些限制。

## 工具

按任务设计，几个相近的接口合成一个工具（例如移动、复制、改名是一个 `move_files`），网页上能做的项目、节点和面板管理操作都有对应的工具（不提供的见[还没有的](#还没有的)）。工具按令牌的权限给，只读令牌只看到 20 个左右，勾满权限的管理员令牌看到全部。项目、容器、节点都可以用名称或 ID 指定（项目名可以重名，重名时要求用网络名或 ID）。工具说明是英文的（给模型看），返回 JSON。令牌没有相应权限（或者是备用面板）时，客户端拿到的工具列表里就没有修改类工具、`exec` 和 `node_exec`。

### 查看

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `overview` | 节点在线数、容器运行数、需要处理的事（离线节点、没在运行的容器、可更新的镜像、从未部署的容器数） | `ListNodes`、`ListProjects` |
| `list_projects` | 项目、所在分组和每个容器的状态、所在节点，可以只列一个分组的 | `ListProjects` |
| `get_project` | 修订号、快照间隔和份数上限、内部端口、容器、端口映射、已连接项目、共享变量名（有草稿时另列草稿里的变量名） | `GetProject` |
| `get_container` | 配置（compose、命令、健康检查按原样返回；变量只给名称，另列哪些是密钥；git 地址去掉账号和令牌）、草稿差异、最近 5 个版本、最近 20 条事件、运行状态 | `GetProject`、`ListVersions`、`ListContainerEvents` |
| `list_deployments` | 项目的部署记录：时间、来源、涉及的容器和版本、是否成功 | `ListDeployments` |
| `check_images` | 立即检查项目里的镜像有没有新版本 | `CheckImages` |
| `list_registries` | 项目登录过的私有镜像仓库地址，不返回密码 | `ListRegistries` |
| `list_backup_snapshots` | 一个备份节点（`backup_node`，默认第一个）上副本里的快照（截至上次同步） | `ListBackupSnapshots` |
| `list_nodes` | 节点、标签、在线、占用、容器数，可以只列带某个标签的 | `ListNodes` |
| `query_metrics` | 节点或容器在一段时间内（默认 60 分钟，最长 7 天）的 CPU、内存、磁盘、网络、重启次数：最小、平均、最大、最新和不超过 60 个点 | `QueryNodeMetrics` |
| `get_logs` | 最近的日志，默认 200 行、最多 1000 行，可以只看 stderr、按关键字过滤 | `StreamLogs`（不跟随） |
| `list_snapshots` | 容器的快照：ID、类型、时间、大小，标出能不能用 `restore_snapshot` 恢复、能不能删除，以及项目的份数上限 | `ListSnapshots` |
| `check_routes` | 端口映射的域名检查：是否解析到入口节点、证书状态。用面板自己的检查，不按 agent 给的地址发请求，不能被拿来探测内网 | `CheckDomains` |
| `query_events` | 项目的活动记录，可以按容器、只看失败的筛选，翻页 | `ListProjectEvents` |
| `inspect_node_storage` | 只给管理员（内容涉及节点上所有项目）：Docker 镜像、容器可写层、数据卷、构建缓存的占用和可回收空间，最大的镜像，每个容器的 data、volumes、快照占用。只读，不清理 | `GetNodeStorage`（节点的 `GetStorage`） |
| `list_versions` | 容器的全部版本：创建和部署时间、镜像、git 提交、是否保存了文件、回滚自哪个版本（`get_container` 只给最近 5 个） | `ListVersions` |
| `get_node` | 一个节点的详情：系统和版本、占用、标签、告警规则、项目根目录、节点访问是否可用、面板不认识的容器和以节点为准的项目（见下面的 `adopt_*`）、信任的面板、节点上保存的副本 | `ListNodes`、`ListNodePanels`、`ListNodeReplicas` |
| `check_mesh` | 一个节点（或面板自己）看到的节点网络：每个成员的路径（直连、中转）、延迟、丢包、最近握手；带 `peer` 时让节点立即 ping 这个成员 | `GetNodeMesh`、`GetPanelMesh`、`PingNodeMesh` |
| `get_node_mirrors` | 只给管理员：节点的镜像加速地址、Docker 当前生效的和默认的加速地址 | `GetNodeMirrors` |
| `list_node_images` | 只给管理员：节点上的镜像（标签、ID、大小、创建时间、使用它的容器、Nagi 需要它的原因），一键清理会删除几个、至少释放多少，可回收的构建缓存。可以按文字筛选，最多列 300 个 | `ListNodeImages` |

### 日常操作

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `create_project` | 新建项目（不带容器） | `CreateProject` |
| `update_project` | 修改项目名称、网络名、定时快照（开关、间隔或每天定时）、份数上限、内部端口（`容器:端口` 或 `服务.容器组:端口`，UDP 加 `/udp`）、出站限制（模式和放行地址，只有管理员能改，接口检查），立即生效。`expected_revision` 是 `get_project` 返回的项目修订号 | `UpdateProjectSettings` |
| `set_alert_rules` | 修改项目里容器的告警规则（重启、5xx、4xx、健康检查、快照、备份同步、CPU、流量、出站拦截），或者给一个容器单独设置；`use_project` 让容器改回用项目的。只改传了的项 | `UpdateProjectSettings`、`SetContainerAlertRules` |
| `update_shared_env` | 设置、删除共享变量，或标为密钥，存为草稿，返回引用了改动变量的容器。用 `deploy` 的 `apply_shared_env` 生效 | `SaveSharedEnv` |
| `discard_shared_env` | 丢弃共享变量的草稿 | `DiscardSharedEnv` |
| `connect_project`、`disconnect_project` | 连接、断开两个项目，立即生效。连接后用 `容器.网络名` 访问对方的容器，容器组里的服务是 `服务.组.网络名` | `ConnectProject`、`DisconnectProject` |
| `login_registry`、`logout_registry` | 登录、退出项目的私有镜像仓库。密码存在面板上，不返回 | `LoginRegistry`、`LogoutRegistry` |
| `add_container` | 用镜像或 compose 添加容器，节点默认和网页一样选项目里容器最多的在线节点；默认不部署，要部署时传 `deploy` 或再调用 `deploy`。镜像表单的端口第一次部署时导入成映射、对外开放（不认 `host_ip`，只导入一次），项目里的容器互相访问不需要。添加时就能用 `env_secret` 把变量标为密钥，容器组可以带 `service_env`。照常保存，结果里带 `warnings`（见[给 agent 的说明](#给-agent-的说明)） | `AddContainer`、`Deploy` |
| `update_container` | 修改配置存为草稿，覆盖网页上容器配置的所有项：镜像、启动命令、端口、持久化目录、工作目录、资源限制、健康检查，或整份 compose；容器变量（设置、删除、标为密钥）、容器组的服务级环境变量；git 来源、保留路径、有无状态、部署依赖。返回改了哪些，不部署。带 `expected_revision`（`get_container` 返回的修订号）时，期间被别人改过就拒绝。`add_container` 也接受同样的配置项。结果里带 `warnings`：只检查这次改到的部分（新的 compose、改了的端口、这次设置的变量值、镜像和有无状态），只改变量时不重复提醒 compose 原有的写法 | `SaveDraft` |
| `set_display_name` | 修改容器的显示名称 | `SetContainerDisplayName` |
| `set_project_group` | 只给管理员：把项目放进分组（新名称即新建），或移出分组。会改变成员的访问范围（见 [panel.md#分组和标签](panel.md#分组和标签)） | `SetProjectGroup` |
| `set_node_tags` | 只给管理员：替换节点的标签（最多 10 个） | `SetNodeTags` |
| `set_snapshot_schedule` | 给容器单独设置定时快照（开关、间隔或每天定时），或改回用项目的。有备份节点或顶替的容器关掉定时快照时，结果的 `warnings` 提醒（备份节点在每次定时快照后同步）；`set_backup_node` 添加备份节点时同样检查 | `SetContainerSnapshotSchedule` |
| `discard_draft` | 丢弃草稿 | `DiscardDraft` |
| `deploy` | 部署一个或多个容器（有草稿的用草稿）。过程中用进度通知推送部署事件，结束后返回结果和失败时的最后 50 行输出；超过 10 分钟返回「仍在进行」，之后用 `get_container` 查。`expected_revisions` 同上；`wait_healthy_seconds`（最多 300）在部署成功后等容器运行、有健康检查的变成 healthy，不健康或停了算不通过，只回报，不自动回滚；没有健康检查的服务在 `health.note` 里注明只确认了在运行。`apply_shared_env` 先让共享变量的草稿生效，不指定容器时只生效、不部署。成功和失败都返回 `warnings`：部署日志里的提醒行（以「注意：」「警告：」开头的，端口导入和没导入的，保留路径覆盖了上传内容的），以及部署前就能看出的（有共享变量草稿却没带 `apply_shared_env`，git 来源从没拉取过却没带 `pull_git`） | `Deploy`、`GetProject` |
| `restore_snapshot` | 从定时或手动快照恢复数据。不带 `confirm` 只返回预览（会发生什么、数据会怎样）；`confirm` 原样填快照 ID 才执行。执行前先给当前数据拍一份保护快照，拍失败就不恢复；保护快照会让要恢复的快照因份数上限被删掉时，直接拒绝。恢复时停止容器、替换 data/ 和 volumes/，配置不变 | `ListSnapshots`、`CreateSnapshot`、`RestoreSnapshot` |
| `control_container` | 启动、停止、重启。带 `service` 时只控制容器组里的这个服务（服务名见 `get_container` 的 `services`），其他服务不动；顶替的规则按整个容器算 | `ControlContainer`、`ControlService` |
| `rollback` | 回到某个版本，只回配置和文件。带 `restore_data` 时同时把数据恢复到这个版本被替换之前，要先预览、`confirm` 原样填版本号才执行 | `Rollback` |
| `create_snapshot` | 立即做一次快照 | `CreateSnapshot` |
| `delete_snapshot` | 删除定时、手动或部署前快照，版本文件不能删。和恢复一样分两步：不带 `confirm` 只返回预览，`confirm` 原样填快照 ID 才删除 | `ListSnapshots`、`DeleteSnapshot` |
| `save_route` | 新建或修改端口映射（域名、HTTPS、TCP/UDP、PROXY protocol、跨节点入口）。HTTP 服务用它绑域名，部署过的容器改对外端口也用它（镜像表单和 compose 的 ports 只导入一次） | `SaveRoute` |
| `delete_route` | 删除端口映射，立即生效。不带 `confirm` 只返回预览，`confirm` 原样填映射名才删除 | `DeleteRoute` |
| `retry_cert` | 让入口节点立即重新申请域名的证书。不指定节点时，对证书还没拿到的入口节点都重试 | `CheckDomains`、`RetryDomainCert` |
| `sync_backup` | 开始依次同步到所有备份节点（或只同步 `backup_node`）。同步在源节点上进行、不限时，工具最多等 `wait_seconds`（默认 30，最多 600）后返回各自的状态和进度（`bytes_done`、`bytes_total`、`bytes_per_second`、`eta_seconds`、重试次数），没传完时说明仍在进行，之后用 `get_container` 查；`stop` 停止正在进行的同步，已传的文件留着，下次接着传（见 [snapshot.md](snapshot.md#同步的进度)） | `SyncBackup`、`StopBackupSync`、`GetProject` |
| `set_node_alert_rules` | 只给管理员：修改节点的告警规则（内存、磁盘、离线、证书、CPU） | `SetNodeAlertRules` |
| `pull_node_image` | 只给管理员：在节点上拉取镜像，用节点全局的 Docker 登录信息和镜像加速，等拉完才返回，最长 30 分钟 | `PullNodeImage` |
| `exec` | 在容器里执行一条命令，不分配终端、没有输入，默认 30 秒、最长 120 秒超时，输出最多 64 KB（超出时保留开头和结尾）。权限和终端一样，只读成员不能用，操作员不能进入特权容器；令牌要勾「执行命令」 | 节点的 `Exec`（和 `/ws/exec` 同一段代码） |

### 不可逆或会中断服务的操作

都分两步：不带 `confirm` 只返回预览（会发生什么、会丢什么），agent 给用户看过、用户同意后，再把 `confirm` 原样填成说明里写的值调用一次才执行。都标为 `destructiveHint`。

| 工具 | 内容 | `confirm` 填 | 用到的接口 |
| --- | --- | --- | --- |
| `delete_container` | 停止并删除容器，连同版本和端口映射。容器目录默认保留，`delete_dir` 时一起删除。预览列出相关的端口映射和依赖它的容器 | 容器名 | `DeleteContainer` |
| `delete_project` | 删除项目和全部容器。容器目录默认保留，`delete_dirs` 时一起删除 | 项目名 | `DeleteProject` |
| `restore_deployment` | 回到某次部署时的状态：版本不一样的容器回滚到那次的版本，一起部署，不恢复数据。预览列出每个容器的版本变化 | 部署记录 ID | `ListDeployments`、`RestoreDeployment` |
| `set_backup_node` | 添加一个备份节点（立即生效，不用确认），或者带 `remove` 移除一个；移除要先预览，`delete_replica` 时同时删除那里的副本。容器可以有多个备份节点，也可以和顶替同时设置 | 容器名（移除时） | `SetBackupNodes` |
| `restore_from_backup` | 原节点离线时，在一个备份节点上用副本里的快照把容器恢复出来，之后写入的数据会丢。`backup_node` 不填时用在线的、最近同步成功的那个。预览写明用哪个节点、哪一份快照 | 容器名 | `ListBackupSnapshots`、`RestoreFromBackup` |
| `migrate_container` | 把容器迁移到另一个节点，不丢数据，最后一次同步时短暂停止。两个节点都要在线 | 目标节点名 | `MigrateContainer` |
| `redeploy_elsewhere` | 原节点离线、又没有副本时的最后手段：在另一个节点上重新部署，数据为空、旧版本不能回滚 | 目标节点名 | `RedeployElsewhere` |
| `set_failover` | 设置顶替节点和见证人。有状态的容器默认等 `confirm_takeover` | 顶替节点名 | `SetFailover` |
| `clear_failover` | 取消顶替 | 容器名 | `ClearFailover` |
| `confirm_takeover` | 有状态容器的节点失联、热备在等确认时，让热备用最近同步的数据顶替 | 容器名 | `ConfirmTakeover` |

| `delete_node_images` | 只给管理员：删除节点上的镜像（连同全部标签，`images` 填标签或至少 12 位的 ID）或单个标签（`tags`）。预览列出会删除的和节点会拒绝的（有容器使用、Nagi 需要） | 预览给的确认码 | `ListNodeImages`、`DeleteNodeImages` |
| `prune_node_images` | 只给管理员：一键清理节点上没有容器使用、Nagi 也不需要的镜像，`build_cache` 时（默认不）同时清理构建缓存。预览列出镜像和至少释放的空间 | 预览给的确认码 | `ListNodeImages`、`PruneNodeImages` |

`change_witness`（更换见证人，顶替照常进行）不中断服务，不用确认。`get_container` 返回备份节点的同步状态（正在同步时带进度 `progress`，用户停止的标 `stopped`）、单独指定的副本存放位置（`replica_location`）和顶替的状态（在哪个节点上运行、最近一次同步、是否在等确认）。

### 文件

读写容器卷里的文件，和网页的文件管理走同一条路：经 mesh 传给容器所在的节点，节点把操作限制在卷里。归在「执行命令」权限下（能执行命令本来就能读写容器里的文件）：只读成员不能用，令牌要勾「执行命令」，访问范围外的项目不能用，备用面板不能写。

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `list_files` | 不带 `volume` 时列出容器的卷（`.data` 是容器的 data/ 目录，git 拉取、上传、回滚时会被替换，保留的路径除外）；带上时列出卷里的一个目录，带 `snapshot_id` 时列出快照里的，带 `sizes` 时统计每个子目录的大小 | `ListVolumes`、`ListVolumeFiles`、`ListVolumeDirSizes` |
| `read_file` | 读一个文件，最多 256 KB，返回内容和 SHA-256。默认只读文本；`encoding` 为 `base64` 时任何文件都按 base64 返回。目录拒绝 | 节点的 `VolumeDownload` |
| `write_file` | 新建或整个覆盖一个文件，最多 1 MB：文本放 `content`，二进制（例如要用 `extract_file` 解开的压缩包）放 `content_base64`。文件已经在时必须带 `expected_sha256`（`read_file` 返回的），期间被改过就拒绝，原来不在的文件期间被建了也拒绝（节点在写入前、持有容器锁时核对）；已有的文件超过 1 MB 时不能覆盖。`.data` 里的文件在 git 拉取、上传、回滚时被替换（保留路径除外）。写的是 `.env` 这类文件时，结果的 `warnings` 提醒改用变量：Nagi 不会把它传给 compose | 节点的 `VolumeUpload` |
| `move_files` | 在一个卷里移动、复制（`action` 为 `move`、`copy`）或原地改名（`rename`）。同名时默认什么都不改、返回冲突的名字，agent 再选 `conflict`：`rename`（保留两者）、`skip`、`overwrite`。覆盖会丢掉目标位置原来的内容，先预览，`confirm` 填预览给的确认码才执行 | `RenameVolumeFile`、`MoveVolumeFiles`、`CopyVolumeFiles` |
| `compress_files` | 把同一个目录里的几项打包成 zip 或 tar.gz，放在同一个卷里。同名的处理同上 | `CompressVolumeFiles` |
| `extract_file` | 解开 zip、tar.gz、tar：默认解到以压缩包命名的新目录（和网页一样），`merge` 时解到已有的目录里。同名的处理同上 | `ExtractVolumeFile` |
| `create_dir` | 在卷里新建目录（连同上级目录） | `CreateVolumeDir` |
| `restore_file` | 把定时、手动或部署前快照里的一个文件或目录恢复到原位，不停容器：快照里有的文件覆盖当前的，别的不动。不带 `confirm` 只返回预览，`confirm` 原样填快照 ID 才执行 | `ListSnapshots`、`RestoreVolumeFile` |
| `delete_file` | 删除卷里的一个文件或目录（连同里面的内容）。不带 `confirm` 只返回预览，`confirm` 原样填路径才删除 | `DeleteVolumeFile` |
| `get_download_link` | 只给管理员。给一个文件或目录（打包成 .tar.gz，`snapshot_id` 时是快照里的）的直连下载，交给别的机器或下载工具：HTTPS 能用时是 5 分钟有效的链接（`method: https`）；HTTPS 不能用、节点改用[加密 HTTP](agent.md#加密-http-下载) 时是下载命令（`method: encrypted_http`，`command_posix` 是 Linux、macOS 的 curl + openssl，`command_windows` 是 Windows 的 PowerShell 脚本；大文件分 4 段同时下载；带 SHA-256 或不校验的原因）；命令只能在第一次运行它的机器上用（链接绑定第一次下载的来源地址），文件在下载中被修改时中断；关了直连下载或都不能用时报错，改用网页下载。规则和网页的菜单一样 | 面板签发，节点的 `PrepareDirectHttp` |

`get_download_link` 的命令里有解密的密钥：它出现在对话里（工具说明写明了），运行后留在 shell 历史中，只交给用户。和网页一样每次签发记一次 `DownloadVolumeFile`（参数里有链接编号 `direct_link`，命令另有 `direct_http`），工具调用的记录不含返回的结果，密钥不进活动记录。

读写都记进活动记录（`DownloadVolumeFile`、`UploadVolumeFile`、`CreateVolumeDir`、`RestoreVolumeFile`、`DeleteVolumeFile`、`RenameVolumeFile`、`MoveVolumeFiles`、`CopyVolumeFiles`、`CompressVolumeFiles`、`ExtractVolumeFile`），带着卷和路径。`create_dir`、`restore_file`、`delete_file`、`move_files`、`compress_files`、`extract_file` 会修改卷，令牌还要勾「可修改」。移动、复制、打包、解压在节点本地进行，过程中用进度通知推送已处理的文件数和字节数；和部署一样用不跟着客户端取消的 ctx，超过 10 分钟返回「仍在进行」。文件内容和日志一样当作不可信数据。

网页的上传、下载走 HTTP（分块上传、断点续传、目录打包下载），MCP 不能传大文件：读写都以 1 MB 以内为限（读 256 KB），更大的文件用 `exec` 在容器里处理（例如从可信的地址下载），或者在网页上传。项目压缩包的上传部署（`/api/upload`）同样没有对应的工具，可以用 `write_file` 写入不超过 1 MB 的压缩包再 `extract_file`，或者改用 git 来源。

### 节点管理

和网页的节点页同一组接口。查看类（`get_node`、`check_mesh`）所有角色都能用，访问范围照常生效；修改类只有管理员能用（操作员看不到这些工具），令牌要勾「可修改」。会让面板忘记节点、丢掉数据或者牵涉安全判断的，分两步，不带 `confirm` 只返回预览。

| 工具 | 内容 | `confirm` 填 | 用到的接口 |
| --- | --- | --- | --- |
| `rename_node` | 修改节点在面板上的名称 | — | `RenameNode` |
| `rename_node_tag` | 给所有节点改标签名；`new_name` 为空时删除标签（要确认），访问范围随之改变 | 标签名（删除时） | `RenameNodeTag` |
| `rename_project_group` | 改项目分组名；`new_name` 为空时删除分组（要确认），项目变为未分组 | 分组名（删除时） | `RenameProjectGroup` |
| `approve_node` | 批准用接入码加入、在等批准的节点。预览给出节点报告的主机名、地址、系统、版本（标明是节点报告的，不可信），提醒接入码可能泄露 | 节点名 | `ApproveNode` |
| `remove_node` | 让面板忘记节点；节点上还有容器时接口拒绝 | 节点名 | `RemoveNode` |
| `set_node_mirrors` | 替换节点的镜像加速地址（写 `/etc/docker/daemon.json`，再让 Docker 重新加载配置，不重启容器；重新加载不了时要重启 Docker 才用） | — | `SetNodeMirrors` |
| `set_node_projects_root` | 修改节点的项目根目录，只影响新建的容器 | — | `SetNodeProjectsRoot` |
| `set_node_mesh_options` | 关闭 QUIC 快速通道、不替别的节点中转、关闭直连下载、设置 HTTPS 不能用时加密 HTTP 下载的端口（`direct_http_port`，0 是默认的 80，见 [agent.md](agent.md#加密-http-下载)），只改传了的项 | — | `GetNodeMesh`、`SetNodeMeshOptions` |
| `test_mesh_speed` | 节点和一个成员之间测速（几秒的真实流量），返回上传、下载（MB/s），走 QUIC 时附 WireGuard 的对比 | — | `TestNodeMeshSpeed` |
| `upgrade_nodes` | 把一个节点（`node` 填节点名）或所有落后的节点（`all`）升级到面板的版本，后台进行，用 `list_nodes` 看版本，用 `check_panel_update` 看各节点的阶段和下载进度；`cancel` 取消排队和下载中的升级（不用确认） | 节点名或 `all` | `UpgradeNode`、`UpgradeAllNodes`、`CancelNodeUpgrade` |
| `reclaim_node_snapshots` | 立即清理节点上每个容器的快照仓库，回收没用到的数据，快照本身不删 | — | `ReclaimNodeSnapshots` |
| `remove_node_panel` | 从节点的信任列表移除一个面板；移除的是本面板时本面板忘记这个节点 | 面板指纹 | `ListNodePanels`、`RemoveNodePanel` |
| `delete_node_replica` | 删除节点上已经没有容器使用的副本（`get_node` 里 `in_use` 为 false） | 容器名 | `ListNodeReplicas`、`DeleteNodeReplica` |
| `adopt_node_container` | 把节点上有、面板不认识的容器按节点缓存加入项目 | — | `AdoptNodeContainer` |
| `adopt_stale_project` | 节点报告项目被别的面板改过时，以节点为准重建面板上的记录。节点可能被攻破，预览里写明 | 项目名 | `AdoptStaleProject` |

### 节点文件和节点级操作

节点上的文件和网页上要先做高权限确认的几个操作，等于节点的 root，和 `node_exec` 一样只给管理员、令牌要勾「节点终端」，备用面板上没有。规则和网页的节点文件一样（见 [node-access.md](node-access.md#文件)）：路径先解析成真实路径，`/proc`、`/sys`、`/dev`、Nagi 的数据目录禁止访问，容器目录只读，敏感文件可以改，节点在本地关闭了节点访问时拒绝。

- **高权限确认**：令牌做不了，规则见[高权限确认](#高权限确认)：只有勾了「节点终端」的管理员令牌能调用（修改还要「可修改」），这时算确认过。其他令牌调用时角色检查直接拒绝。
- **每次修改都要确认**（新建目录除外）：不带 `confirm` 只返回预览（节点、路径、大小，敏感路径另有提醒），`confirm` 填预览给的确认码才执行。[确认码](#确认码)由工具、节点和参数算出，和 `node_exec` 一样只保证执行的就是预览过的操作。确认后调用接口时带上 `confirmed`（敏感路径要用户确认过才能改）。
- 读写都和网页一样记进活动记录（带路径，不记内容）；`HandoverNodes` 不提供（见[还没有的](#还没有的)）。

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `list_node_files` | 列出节点上的一个目录：类型、大小、权限、属主、访问规则（`normal`、`confirm`、`readonly`、`denied`），`sizes` 时统计子目录大小 | `ListNodeDir`、`ListNodeDirSizes` |
| `read_node_file` | 读一个文本文件，最多 256 KB，返回内容和 SHA-256；二进制文件只返回属性 | `ReadNodeFile` |
| `get_node_download_link` | 节点上一个文件或目录的直连下载链接或命令，规则同 `get_download_link`，记 `DownloadNodeFile` | 面板签发，节点的 `PrepareDirectHttp` |
| `write_node_file` | 新建或覆盖一个文本文件，最多 1 MB。覆盖时要带 `read_node_file` 返回的 `expected_sha256`，旧内容按网页的规则保留备份。路径在节点的项目根目录（容器目录）里时，预览的 `warnings` 提醒：`compose.yml`、`override.yml`、`.env` 这些每次部署都重写，改动绕过草稿、版本和顶替 | `ListNodeDir`、`WriteNodeFile` |
| `create_node_dir` | 新建目录（连同上级目录），不用确认 | `CreateNodeDir` |
| `delete_node_file` | 删除文件或目录（连同里面的内容），符号链接只删链接，不备份 | `ListNodeDir`、`DeleteNodeFile` |
| `move_node_files` | 移动、复制、改名，同名的处理和 `move_files` 一样 | `RenameNodeFile`、`MoveNodeFiles`、`CopyNodeFiles` |
| `compress_node_files`、`extract_node_file` | 打包、解压 | `CompressNodeFiles`、`ExtractNodeFile` |
| `restore_node_file` | 不带 `backup_id` 时列出文件保留的旧版本；带上时恢复这一版（要确认） | `ListNodeFileBackups`、`RestoreNodeFileBackup` |
| `import_compose_dir` | 把节点上现有的 compose 目录导入为项目里的容器组，数据留在原处。不带 `confirm` 时读目录，返回导入后的 compose、变量名（不给值）、原来运行着的 compose 项目和注意事项；确认后才导入，不部署。预览和结果里带 `warnings`：写死的环境变量、原目录以外的宿主机路径这类（compose 是用户原有的，不提醒单个服务和 `env_file`） | `InspectComposeDir`、`ImportComposeDir` |
| `isolate_node` | 隔离可能被攻破的节点（`undo` 解除），`confirm` 填节点名 | `IsolateNode`、`UnisolateNode` |
| `confirm_pending_ha` | 让从节点缓存拼回的顶替和备份节点设置生效（`discard` 放弃），预览列出涉及的节点，配置有特权时醒目提示，`confirm` 填容器名 | `ConfirmPendingHA` |

### 节点镜像

和节点页的「镜像」标签同一组接口（见 [agent.md](agent.md#节点镜像)），能不能删由节点判断，有容器使用的、Nagi 需要的一律拒绝。

- 只给管理员的令牌。`list_node_images` 不用额外权限；`delete_node_images`、`prune_node_images`、`pull_node_image` 要勾「可修改」，备用面板上没有。
- 网页上删除和清理要先做高权限确认，令牌做不了交互式的确认（规则见[高权限确认](#高权限确认)），改由预览和[确认码](#确认码)代替：不带 `confirm` 只返回预览，用户同意后带预览给的确认码再调用一次。确认码由节点和要删的镜像（删整个镜像时连同它当时的标签；清理时是每个可清理镜像的 ID 和标签、是否清理构建缓存）算出，预览之后节点上的镜像变了（包括多了标签）就对不上，只会再返回一次预览。删整个镜像时还把预览时的标签带给节点，节点发现多了标签就跳过并报告，不会把预览之后新加的标签一起删掉。预览里的 `warnings` 列出可能是回滚要用的旧版本（和本节点上 Nagi 容器正在用的镜像同一个仓库）和本地构建的镜像（没有仓库摘要，删除后拉不回来），只提醒，不阻止。所以管理员的、勾了「可修改」的令牌调用这两个接口时算确认过（见[高权限确认](#高权限确认)），令牌只在 `/mcp` 有效，到这里的令牌请求都来自这两个工具。和 `node_exec` 一样，确认码只保证执行的就是预览过的内容。
- `pull_node_image` 用节点全局的 Docker 登录信息和镜像加速拉取，等拉完才返回，最长 30 分钟。不用确认。每个节点同时最多拉取 2 个，拉取中磁盘剩余空间不足时停下。
- 删除、清理、拉取都记进审计，挂在这次工具调用下面。

### 节点命令

`node_exec` 以 root 身份在节点上执行一条命令，等它结束，返回输出（最多 64 KB）和退出码。默认 30 秒、最长 120 秒超时，到时结束整个进程组。这等于把整台机器交给 agent，所以比网页的节点终端还多几道限制：

- **最后才用**：工具说明和给 agent 的说明里都写明，先用项目、容器、文件和 `exec` 这些工具，做不到或者用户明确要求时才用 `node_exec`。`reason` 必填，写为什么别的工具做不到（或者是用户要求的），显示在预览里。
- **每次都要确认**：不带 `confirm` 只返回预览：节点、命令、工作目录、超时和理由。agent 给用户看过、用户同意后，用同样的参数再调用一次，`confirm` 填预览给的确认码才执行。[确认码](#确认码)由节点、工作目录、超时和命令算出，预览之后改了其中任何一项都对不上，只会再返回一次预览。确认码只保证执行的就是预览过的命令，不能证明用户同意过。用户同意靠 agent 的说明和客户端对 `destructiveHint` 工具的确认提示，令牌本身就有执行命令的权限，所以「节点终端」只在信任这个客户端时勾选。

- 只有管理员能用，令牌要勾「节点终端」：个人访问令牌生成时勾选（生成前要做高权限确认），或者在 OAuth 授权页勾选（允许前同样要做高权限确认）。没勾的令牌看不到这个工具。
- 节点在本地关闭了节点访问（`nagi node access disable`）时拒绝，面板没法替节点打开。备用面板上没有这个工具。
- 每次执行都记进活动记录（`NodeExec`，详情是命令和退出码），并向告警配置里的渠道发一条通知（不带命令内容）；节点也记一条本地日志，带面板指纹、用户名和令牌名。
- shell 和环境变量和节点终端一样（见 [node-access.md](node-access.md#终端)）：root 的登录 shell，`sh -lc` 执行，不分配终端，没有输入。节点跑在容器里时，命令进的是这个容器，返回里会注明。
- 命令里有 `docker run`、`docker compose up/stop/rm` 这类直接改动容器的命令，或者 `docker login` 时，预览的 `warnings` 提醒改用 `update_container`、`deploy`、`control_container` 和 `login_registry`。只提醒，确认后照常执行。
- 和 `exec` 共用每分钟 10 次的限额。节点要升级到支持 `NodeExec` 的版本，旧节点返回「节点版本过旧」。命令的输出和日志一样当作不可信数据。

### 副本的存放位置

修改副本存放在节点上的哪里（见 [snapshot.md](snapshot.md#副本的存放位置)）。这等于让节点以 root 在任意位置建目录、写数据，和 `node_exec` 同一级：

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `set_node_replica_dir` | 修改节点的副本目录（`dir` 为空是默认的 `<节点目录>/replicas`），节点把默认目录里的副本移过去。预览列出会移动的副本 | `ListNodes`、`ListNodeReplicas`、`SetNodeReplicaDir` |
| `set_replica_location` | 修改容器在它的一个备份节点上的副本存放位置（`dir` 为空移回节点的副本目录），节点把副本移过去 | `ListProjects`、`GetProject`、`ListNodes`、`SetBackupReplicaDir` |

- 只有管理员能用，令牌要勾「可修改」和「节点终端」，没勾的令牌看不到这两个工具；备用面板上没有。节点要升级到支持选择存放位置的版本，否则直接拒绝。
- **每次都要确认**：和 `node_exec` 一样，不带 `confirm` 只返回预览（节点、容器、当前和新的位置），用户同意后用同样的参数再调用一次，`confirm` 填预览给的确认码。确认码由工具、节点、容器和整理过的目录算出，改了其中任何一项都对不上。
- **接口上的高权限确认**：见[高权限确认](#高权限确认)，这两个接口要管理员的、勾了「可修改」和「节点终端」的令牌。
- 移动的过程作为进度通知发给客户端，结果里有成功与否和最后几行输出。和网页一样记进活动记录（`SetNodeReplicaDir`、`SetBackupReplicaDir`）。添加备份节点（`set_backup_node`）时不能指定存放位置，加好以后用 `set_replica_location`。

### 管理面板

面板的设置页里能交给 agent 的部分。只给管理员，令牌要勾「管理面板」（修改的还要勾「可修改」），备用面板上只有查看类的。**每次调用都发一条告警**「访问令牌使用了管理面板的工具 `<工具名>`」（带令牌名、成员和来源，和节点命令一样），令牌泄露时用户马上知道。改变面板地址、通知去向或者更新面板的，分两步：不带 `confirm` 只返回预览，`confirm` 填预览给的确认码（`update_panel` 填目标版本）才执行。

| 工具 | 内容 | 用到的接口 |
| --- | --- | --- |
| `get_panel_settings` | 面板的对外地址、端口、指纹、版本、绑定的域名、主密钥文件的路径，备用面板（备用面板上是主面板和同步状态） | `GetSettings`、`GetStandbys` |
| `update_panel_settings` | 修改对外地址、绑定或取消域名（要确认）。生效前先用现在的告警渠道发通知（接口本来就这样，网页上改也一样）。安全入口不能改 | `UpdateSettings`、`SetPanelDomain` |
| `get_alert_channels` | 告警渠道：名称、类型、指向哪里（webhook 只给主机，地址和机器人令牌一律不返回）、webhook 是否允许内网地址 | `GetAlertConfig` |
| `update_alert_channels` | 添加、修改、改名、删除渠道，webhook 可以设置「允许内网地址」（`allow_private`，见 [monitoring.md](monitoring.md#告警)）。除了改名都会改变通知去向，要确认（改「允许内网地址」也是）；生效前先用原来的渠道发通知（接口本来就这样）。webhook 地址和机器人令牌只写，审计里也隐去 | `GetAlertConfig`、`UpdateAlertConfig` |
| `test_alert_channel` | 用保存了的渠道发一条测试消息。不按 agent 给的地址发请求 | `GetAlertConfig`、`TestAlertChannel` |
| `list_activity` | 整个面板的活动：操作和告警，翻页 | `ListActivity` |
| `query_audit` | 按时间、成员、来源、类别、项目、节点、只看失败、关键字查审计记录，翻页；或者取一条记录和它的上下级 | `ListAuditEvents`、`GetAuditEvent` |
| `export_audit` | 按筛选条件导出 JSON Lines，最多 256 KB（超出时截断，只留完整的行） | `ExportAuditEvents` |
| `verify_audit` | 校验审计日志的哈希链，返回结果和最近一次外送的锚点 | `VerifyAudit` |
| `check_panel_update` | 有没有新版本、更新日志、面板和节点的更新进度（下载时带 `progress`：阶段、已下载、总大小、百分比、速度，见 [panel.md](panel.md#更新状态)；旧版本的节点没有）；最新版本是经加速代理查到的时带 `latest_checked_via`（见 [panel.md](panel.md#更新面板)） | `CheckPanelUpdate`、`GetPanelReleaseNotes`、`GetUpdateStatus` |
| `update_panel` | 更新面板到最新版本（要确认），面板重启期间 MCP 暂时不能用 | `CheckPanelUpdate`、`UpdatePanel` |
| `list_members` | 成员、角色、访问范围、是否开了两步验证、通行密钥的名称。只读 | `ListUsers` |

所有令牌都能用的（只影响自己的账号）：`get_project_order`、`set_project_order` 查看和修改自己的项目页排序（`GetProjectOrder`、`SetProjectOrder`），修改仍要「可修改」。

工具都带 MCP 的工具标注（`readOnlyHint`、`destructiveHint`、`openWorldHint`、`idempotentHint`），`_meta.securitySchemes` 里写需要的 OAuth 范围（查看类 `nagi:read`，修改类 `nagi:write`，`exec` 和文件工具是 `nagi:exec`，`node_exec`、节点文件、节点级操作和副本的存放位置是 `nagi:node`，管理面板的是 `nagi:admin`），ChatGPT 按它判断工具要不要授权。`deploy`、`control_container`、`rollback`、`restore_snapshot`、`delete_snapshot`、`disconnect_project`、`delete_route`、`exec`、`node_exec`、`write_file`、`restore_file`、`delete_file`、`move_files`、`extract_file`、`delete_node_images`、`prune_node_images`、`set_node_replica_dir`、`set_replica_location`、`update_panel_settings`、`update_alert_channels`、`update_panel`、节点文件的修改、节点管理里要确认的操作和上面不可逆的操作标为 `destructiveHint`，客户端一般会先问用户。部署这类操作用不跟着客户端取消的 ctx：agent 断开或者等不及，操作在面板上照样做完。

### 还没有的

- **一直不提供**：初始化、登录、高权限确认、改密码、两步验证、通行密钥、会话、OAuth 授权、访问令牌的生成和吊销（生成令牌等于给自己留一个长期凭据），成员的增删、角色和访问范围的修改（等于给自己或别人留后门），邀请、会话策略、审计设置和外送、管理员登录地址、终端录制的回放，以及会把凭据放进对话的操作（邀请链接、节点加入命令 `CreateEnrollCommand`、备用面板配对码和提升、备份密钥 `GetBackupKey`、容器的 webhook 令牌）、安全入口。节点移交（`HandoverNodes`）也不提供：要把另一个面板的接入码交给 agent，执行后本面板忘记这些节点和上面的项目，等于把节点的 root 交给另一个面板，只在网页上做。也不做按 agent 给的地址发 HTTP 请求的探测（会被拿来探测内网），用 `check_routes` 代替。
- **大文件的上传下载**：见[文件](#文件)。日志只取最近的，不跟随（`get_logs`）；终端是交互的，用 `exec`、`node_exec` 执行单条命令代替。

## 给 agent 的说明

agent 常见的乱部署：单个服务也写 compose，密码这类值写死在 compose 里或另写 `.env`，数据挂到容器目录外的宿主机路径，数据库开放端口，用 IP 或 `host.docker.internal` 互相访问。说明分四层，越靠前越短：

- **服务说明**：MCP 初始化时返回，每次连接都发，所以只写一小段：Nagi 的概念（节点、项目、容器、容器组）、修改要先存草稿再部署、部署规则的摘要（每条一句），以及操作员不能保存带特权的配置（[node-access.md](node-access.md#操作员不能借-compose-拿到节点-root)）、当前令牌的角色和权限。部署规则：
  - 单个服务用镜像表单；只有用户给了 compose，或者几个服务必须在同一台机器上一起运行时，才用容器组。
  - 一起工作的容器放在同一个项目里，优先拆成几个容器；只用名字互相访问（`容器`、`服务.组`），不用 IP 和 `localhost`；访问别的项目先 `connect_project`，用 `容器.网络名`。
  - 配置放 Nagi 的变量（`env`、`env_secret`，compose 里写 `${NAME}`，或者共享变量）；普通配置可以写在 compose 里，但优先用变量；密钥（密码、令牌、密钥、连接串里的密码）绝不写死在 compose 里；不写 `.env`、`env_file`。
  - 数据放持久化目录（`volumes`）或相对路径、命名卷，都在容器目录下；不挂容器目录外的宿主机路径。
  - 数据库、缓存这类内部服务不写 `ports`：第一次部署时导入成映射、对外开放，不认 `host_ip`。
  - HTTP 用 `save_route` 绑域名；部署过的容器改对外端口用 `save_route`，不改 ports（只导入一次）。
  - 不用 `host.docker.internal`、`host-gateway`、`172.17.0.1`、局域网 IP 访问节点本机或局域网：新项目默认拦截（[出站限制](agent.md#出站限制)）。
  - 不用 `node_exec` 执行 `docker run`、`docker compose up/stop/rm`，不改容器目录里的 `compose.yml`、`override.yml`、`.env`。
  - 看每次 `add_container`、`update_container`、`deploy` 结果里的 `warnings`，改掉，或者告诉用户为什么保留。
- **资源**：
  - `nagi://docs/containers`：容器的种类、compose 的规则（相对路径、命名卷、端口怎么导入、内部端口的写法）、变量的三层、操作员不能保存的配置，内容摘自 [agent.md](agent.md) 和 [node-access.md](node-access.md)。
  - `nagi://docs/deploy`：部署指南，写全部规则和「Do / Don't」例子：选哪种形式；项目、名字和地址（`服务.组`、`容器.网络名`、`internal_ports` 的写法、容器名不能改）；端口和域名；变量（共享变量要带 `apply_shared_env`、不访问节点和局域网）；数据（持久化目录、不设 `stateful: no`、`workdir` 和保留路径、固定大版本、有备份节点时不关定时快照、`exec` 和 `write_file` 的改动会被替换）；镜像和文件（不写 `build`、`login_registry`、`pull_git` 和 deploy key）；Nagi 会处理的 compose 字段（不写 `container_name`、`network_mode: host`、external，不用写 `restart`、`logging`）；修改和部署（修订号、健康检查和 `wait_healthy_seconds`、部署结果的提醒、`depends_on` 只管一起部署时的顺序、内存上限、不用 `node_exec` 动容器）。例子：Web 应用加 PostgreSQL 拆成两个镜像表单容器（对照一份写死密码、挂 `/opt/pg`、开放 5432 的 compose），以及用户给了 compose 时怎么改写成容器组。依据 [agent.md](agent.md)（创建方式、环境变量、部署方式、项目内网、出站限制）、[exposure.md](exposure.md#compose-里写的-ports) 和 [node-access.md](node-access.md)。

  两份都写在 `internal/server/mcp/resources.go` 里随程序编进去，改了这些文档里相应的规则时要跟着改。
- **工具说明**：`add_container`、`update_container`、`deploy`、`save_route`、`connect_project`、`write_file`、`import_compose_dir`、`node_exec`、`write_node_file`、`set_snapshot_schedule` 的说明和相关参数（`image`、`compose`、`env`、`env_secret`、`volumes`、`ports`、`stateful`、`workdir`、`internal_ports`）用一两句话重复关键规则。
- **结果里的提醒**（`internal/server/mcp/advice.go`，compose 用 `spec.AdviseCompose` 读，和权限检查一样经 compose-go 的 `composeModel`，不做变量替换；端口展开后超过上限的不读，见 [agent.md](agent.md#compose-的限制)。一次调用里同一份 compose 只读一遍，各项提醒共用结果；`update_container` 在草稿保存成功后才读，旧的 compose 只在容器部署过、要比较端口时读）：照常保存，只在结果里加 `warnings`（英文，每条说明问题和怎么改，只列变量名、不列值）。管理员可以合法地这样写，所以不拒绝，措辞留余地。

  | 提醒 | 触发条件 | 工具 |
  | --- | --- | --- |
  | 单个服务 | compose 里只有一个服务（工具分不出用户是否明确要了 compose，措辞写「除非用户要的是 compose」） | `add_container`、`update_container` |
  | 写死的密钥 | compose 服务的 `environment` 里写死了值（不是 `${VAR}` 引用、也不是只写名字），并且变量名像密钥（`spec.SecretVarName`，不区分大小写：含 `PASSWORD`、`PASSWD`、`SECRET`、`TOKEN`、`CREDENTIAL`，或者有 `KEY`、`AUTH`、`PWD`、`DSN` 这样的词，例如 `API_KEY`、`PRIVATE_KEY`、`APIKEY`、`BASIC_AUTH`；`KEYBOARD`、`AUTHOR` 和 `*_FILE` 不算）；或者值是带着写死的密码的连接串（`scheme://用户:密码@`，密码是 `${VAR}` 的不算）。普通配置（`NODE_ENV`、`TZ`、`PORT`）不提醒。提醒列出变量名，建议改成 `${NAME}` 并用 `env_secret` 标为密钥 | 同上、`import_compose_dir` |
  | `env_file` | compose 服务用了 `env_file` | `add_container`、`update_container` |
  | 宿主机路径 | bind 的源路径是绝对路径、`~`、`..` 开头，或命名卷用 `driver_opts.device` 绑到宿主机目录；`/etc/localtime`、`/run`、`/var/run`、`/dev` 这类系统路径和用了变量的不算 | 同上、`import_compose_dir` |
  | 内部服务的端口 | 容器端口是 5432、3306、1433、6379、27017、9200、9300、11211、5672、9042 并且写了对外端口（只绑定本机地址的不算） | 同上 |
  | `host_ip` | 端口写了本机地址以外的 `host_ip` | 同上 |
  | 80、443 | 对外端口是 80 或 443（导入时会跳过） | 同上 |
  | 改端口不生效 | 部署过的容器，这次改了镜像表单的端口或 compose 里的端口 | `update_container` |
  | 节点本机、局域网、`localhost` | 能看到的变量值（compose 的 `environment`、`extra_hosts`，这次设置的 `env`、`service_env`）里有 `host.docker.internal`、`host-gateway`、`172.17.0.1`、私网 IPv4，或连接串里的 `localhost`、`127.0.0.1`；`${VAR}` 的值看不到，只查写出来的部分 | `add_container`、`update_container` |
  | `build`、`container_name`、`network_mode: host`、external | compose 里出现这些 | 同上、`import_compose_dir` |
  | 没有固定版本 | 有数据的容器（持久化目录、命名卷、相对路径挂载，或 `stateful: yes`）的镜像没写标签或是 `latest`；写了摘要、用了变量的不算 | `add_container`、`update_container` |
  | 有数据却无状态 | `stateful` 设为 `no`，但有持久化目录、命名卷或保留路径 | 同上 |
  | `.env` 文件 | 写的文件名是 `.env`、`.env.*`、`*.env` | `write_file` |
  | 部署日志 | 部署日志里以「注意：」「警告：」开头的行、端口导入和没导入的提示、保留路径覆盖了上传内容 | `deploy`、`add_container` 带 `deploy` |
  | 共享变量草稿 | 项目有共享变量草稿，部署时没带 `apply_shared_env` | `deploy` |
  | git 从没拉取 | 有 git 来源、从没拉取过（`last_commit` 为空），部署时没带 `pull_git` | `deploy` |
  | 没有健康检查 | 带 `wait_healthy_seconds` 时，有服务没有 Docker 健康检查：`health.note` 注明只确认了在运行 | `deploy` |
  | 关掉定时快照 | 有备份节点或顶替的容器，定时快照是关的 | `set_snapshot_schedule`、`set_backup_node` |
  | 直接用 docker | 命令里有 `docker run`、`docker compose up/stop/rm/down` 这类，或 `docker login` | `node_exec` 的预览 |
  | 容器目录 | 路径在节点的项目根目录里 | `write_node_file` 的预览 |

  镜像表单的环境变量本来就是 Nagi 的变量，不提醒写死的密钥。`update_container` 只检查这次改到的部分。导入现有目录时 compose 是用户原有的，不提醒单个服务和 `env_file`（`env_file: .env` 由导入换成面板的变量），宿主机路径的提醒改为「快照只能包含原目录的子目录」。

## 安全

- **密钥不进模型**：变量的值、deploy key、webhook 令牌、备份密钥都不返回，只给名称和「已设置」。agent 可以设置变量的值（只写）。compose、命令、健康检查的文字按原样返回，密钥要放在变量里，不要直接写进去。git 地址不能带账号或令牌（私有仓库用 SSH 和 deploy key），早期保存的地址返回时也去掉。
- **不可信内容**：日志和 `exec` 的输出可能含有针对 agent 的指令，工具说明里写明这些是数据，不是指令。
- **限额**：每个令牌每分钟最多 60 次调用，其中 `exec` 最多 10 次；单次返回内容有上限，超出时截断并注明。
- **可以随时止损**：令牌页一键吊销；审计页能按来源筛出 MCP 做的所有操作。
- **被拒绝的调用留痕**：令牌没有权限的工具不在它的工具列表里，客户端照样按名字调用时，SDK 当作不存在的工具拒绝，面板先从请求里认出工具名，记一条「MCP 调用被拒绝」（`MCPToolDenied`）。

## 实现

- `internal/server/mcp`：`/mcp` 入口和令牌核对（`mcp.go`，用 SDK 的 `RequireBearerToken`）、进程内调用面板接口的传输、按令牌的权限给工具（`tools.go`）、工具本身（`list.go`、`ops.go`、`snapshots.go`、`diagnose.go`、`files.go`、`fileops.go` 卷文件的移动打包解压、`alerts.go` 告警规则和版本、`nodes.go` 节点管理、`nodefiles.go` 节点文件和节点级操作、`images.go` 节点镜像、`replicadirs.go` 副本的存放位置、`panel.go` 管理面板和项目页排序）、给 agent 的资源（`resources.go`）、保存后的提醒（`advice.go`，compose 的读法在 `spec.AdviseCompose`）。每个工具声明是否修改、是否执行命令和用到的接口，`tools_test.go` 检查查看类的工具只用查询接口、修改类的至少用到一个修改接口、接口都存在且操作员能调用（声明了只给管理员的除外）、用到要「节点终端」的接口的工具都声明了 `nodeExec`；`nodes_test.go` 用假的接口检查要确认的工具先预览、确认码对上才执行。`internal/server/api/token_elevation_test.go` 按[高权限确认](#高权限确认)的表逐个检查 `elevationRequired` 里的接口；`TestTokenNeverReaches` 逐个检查设置、账号、初始化的接口，令牌只能碰管理面板列出的那些。
- `internal/server/oauth`：授权服务器。`oauth_test.go` 按规范走一遍完整流程（注册、授权、换令牌、刷新、旧刷新令牌重用时吊销整个授权、吊销），并测回调地址不匹配、没有 PKCE、PKCE 不对、授权码重用、拒绝授权、本机回调换端口。
- `internal/server/auth/tokens.go`：生成和核对令牌。`internal/server/api/tokens.go`：令牌管理和授权页的接口。`internal/server/api/exec.go`：`exec` 用的执行命令（和终端同一条路）。`internal/server/api/agentfiles.go`：文件工具用的读写（和网页的文件管理同一条路）。
- `internal/server/store`：`api_tokens`、`oauth_clients` 表。查询手写在 `tokens.go`，不放进 `queries.sql`。
- 界面：账号安全里的「访问令牌」（`pages/tokens.tsx`），成员行的 ⋯ 里的「访问令牌」（管理员查看和吊销），授权页 `/oauth/authorize`（没登录时记下地址，登录后回来），审计页显示来源。
