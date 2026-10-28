# 开发规划

代码层面的约定：仓库结构、依赖库、各开发阶段的完成标准。整体开发顺序见 [README.md](README.md#开发顺序)。

## 基本信息

- Go module：`github.com/SIPC/nagi`
- 许可证：Apache-2.0
- Go 版本：1.26（见 `go.mod`）
- 运行平台：Linux（amd64 / arm64），以 systemd 服务运行

## 仓库结构

按角色划分：`server` 和 `agent` 互不引用。两边都要用的通用代码放在 `internal/pkg`，这里不放业务逻辑。

```
cmd/nagi/             入口，cobra 子命令
internal/
├── proto/            proto 定义及生成代码（面板、节点、浏览器共用）
├── pkg/              通用代码，不含业务逻辑
│   ├── tlsutil/      证书生成、公钥指纹
│   ├── db/           SQLite 打开、迁移
│   ├── tunnel/       TLS + yamux 连接、反向 gRPC
│   ├── alert/        告警发送（webhook、Telegram），面板和节点共用
│   ├── invite/       面板接入码的编码与解析
│   ├── lease/        顶替的租约投票（面板和节点共用）
│   └── mesh/         WireGuard mesh：收发层（直连 / UDP 中转 / TLS 中转）、进程内网络栈、MeshService
├── server/           面板（证书签发在 pkg/tlsutil）
│   ├── api/          Connect-RPC 接口、安装脚本、上传、webhook、终端
│   ├── auth/         登录、TOTP、安全入口
│   ├── nodes/        节点连接管理、上报、mesh、见证人
│   ├── projects/     项目和容器的数据、草稿与版本、部署编排、代理名单、从节点缓存拼回项目
│   ├── sealer/       面板主密钥加密
│   └── store/        面板数据库（sqlc）
└── agent/            节点：连接面板、信任列表、mesh、副本同步、顶替
    ├── project/      容器的部署与删除、配置缓存、项目内网和代理、版本文件、快照、端口映射、override 生成
    ├── compose/      调用 docker compose
    ├── docker/       Docker Engine API：状态、日志、终端、事件、网络、代理容器
    ├── proxy/        内嵌 Caddy
    ├── snapshot/     kopia
    ├── store/        节点数据库（nodestore）和项目数据库（projectstore），sqlc
    ├── metrics/      指标采集与存储
    └── alerts/       告警规则判断、发送、代发与补发
web/                  前端
```

## 依赖库

**后端**

| 用途 | 选择 | 说明 |
| --- | --- | --- |
| 命令行 | cobra | 子命令、帮助、补全；便于以后加重置密码、修改信任列表等本地命令 |
| 面板 ↔ 节点 | gRPC + hashicorp/yamux | 反向 gRPC，见 [control-plane.md](control-plane.md#接口设计) |
| 浏览器 ↔ 面板 | Connect-RPC（connectrpc.com/connect） | 与节点共用 proto |
| proto 工具链 | buf | 生成 Go 与 TypeScript 代码、检查不兼容的改动 |
| SQLite | modernc.org/sqlite + sqlc | 写 SQL，生成强类型 Go 代码；配合简单的迁移脚本 |
| Docker | `docker compose` 命令 + Docker Engine Go SDK | 部署、停止调用 compose 命令，与用户手动操作完全一致；状态、日志、终端、监控、事件用 Engine API |
| 解析 compose | compose-spec/compose-go | compose 官方的解析库，生成 override 前用它读懂用户的 compose |
| 反向代理 | Caddy（作为库内嵌）+ caddy-l4 | 见 [exposure.md](exposure.md) |
| 快照 | kopia（作为库内嵌） | 见 [snapshot.md](snapshot.md) |
| WireGuard | wireguard-go（用户态），自己实现底层收发 | 见 [mesh.md](mesh.md) |
| 日志 | 标准库 log/slog | |

**前端**

- Vite + React + TypeScript，纯静态输出，构建产物通过 `embed` 打包进二进制
- TanStack Router（类型安全路由）+ TanStack Query（接口缓存与刷新）
- Tailwind + shadcn/ui，支持深浅色，适配移动端
- 接口客户端由 buf 从 proto 生成

## 本地开发

需要 Go、Node.js + pnpm、Docker（含 compose 插件）。

```bash
# 生成 proto 代码（Go 与 TypeScript），需要先 go install buf 和 protoc-gen-go / -go-grpc / -connect-go
buf generate

# 生成数据库代码（用 Docker 运行 sqlc，不需要本地安装）
docker run --rm -v "$PWD:/src" -w /src sqlc/sqlc generate

# 构建前端，产物会被打包进二进制
pnpm -C web install && pnpm -C web build

# 构建并运行（单机：面板 + 内嵌节点）
CGO_ENABLED=0 go build -o bin/nagi ./cmd/nagi
./bin/nagi server --data-dir .nagi-dev --port 28443
```

在没法监听 80/443 的开发机上（例如 Windows），可以加 `--no-proxy`，内嵌节点不启动 Caddy。

nagi 要静态编译（`CGO_ENABLED=0`）：项目内网的代理容器用空镜像，运行的是挂进去的 nagi 本身。内嵌 Caddy 后二进制约 90MB；发布构建时加 `-ldflags "-s -w"` 去掉调试信息可以小不少。

前端开发时可以用 `pnpm -C web dev`，它会把接口请求转发到 `https://127.0.0.1:28443`（可用 `NAGI_PANEL` 环境变量修改），浏览器打开 `http://localhost:5173`。

界面规范见 [ui.md](ui.md)。改界面时别漏了各文档里写到的这些提示：

- 网络名默认由项目名生成（中文名要自己填）；改网络名时提示连接了本项目的项目要跟着改；"网络"页列出各容器在连接方看到的地址（[agent.md](agent.md#项目连接)）
- 不在快照范围内的 volume（`external` 或自定义 driver，接口里是容器的 `unmanaged_volumes`）和 `network_mode: host` 的服务（`host_network_services`）醒目提示（[agent.md](agent.md#部署方式)）
- 首页提醒开启两步验证；提醒单独备份面板主密钥文件（设置里的 `master_key_path`，见 [panel.md](panel.md)）
- 端口映射旁给出访问地址（节点列表里的 `address`，见 [exposure.md](exposure.md#端口映射)）；修改容器时对外端口在映射里改，不在容器配置里改
- 顶替和备份节点二选一，设了一个时另一个直接说明原因；设了顶替时迁移先说明要取消顶替（[failover.md](failover.md)）
- 版本文件已经不在了的版本标出不能回滚（[agent.md](agent.md#版本与回滚)）
- 面板自己的设置（账号安全、告警、接入码、面板的 mesh）放在单独的设置页，不和节点列表混在一起

## 发布

版本号从 v0.1.0 开始，按语义化版本号（`v<主>.<次>.<修订>`）打标签：修复问题加修订号，新增功能加次版本号。`nagi version` 和面板显示的版本就是标签名，本地构建显示 `dev`。

从 v0.1.0 起，已安装的面板和节点会直接升级到新版本，改动要兼容已有数据：

- 数据库只新增迁移脚本，不修改已发布的迁移。
- 面板和节点可能暂时不是同一版本（面板先升级，节点随后在界面上升级），proto 只新增字段，不删除、不改编号。
- 节点上缓存的配置、状态文件要能被新版本读取。

推送 `v*` 标签时，`.github/workflows/release.yml` 构建前端、运行 `go test ./...`，编译 `nagi-linux-amd64`、`nagi-linux-arm64`（`-s -w`，`main.version` 为标签名），连同 `scripts/install.sh`、`scripts/uninstall.sh` 和 `SHA256SUMS` 上传到 GitHub Release。

`scripts/install.sh` 是面板的一键安装脚本，从 `releases/latest/download/`（或 `--version` 指定的版本、`--download-url` 指定的镜像）下载上述文件并校验 SHA-256，安装到 `/usr/local/bin/nagi`，写入 systemd 服务 `nagi-server` 并启动，然后从日志中取出面板地址、公钥指纹和初始化链接。文件名是脚本和发布流程之间的约定，改名时两边一起改。

节点安装脚本（面板生成的 `/install.sh`）和面板上的一键升级也从 GitHub Release 下载同样的文件（和面板同一版本，地址见 `internal/pkg/release`），面板不提供二进制。本地编译的面板（`dev`）加入的节点会装上最新发布的版本；要在测试环境里用本地编译的 nagi，把 `nagi-linux-<架构>` 和对应的 `SHA256SUMS` 放到一个 HTTP 目录下，安装命令加 `--download-url <该地址>`。

面板安装脚本的其他要点：

- 已安装时再次执行即为升级，只更换二进制并重启；端口、数据目录、是否带内嵌节点不允许在升级时修改（已加入的节点按原端口连接面板）。
- `--standby <配对码>` 安装备用面板：首次启动带上配对码，配对成功（数据目录出现 `panel/paired`）后从服务配置中去掉。
- 本机已有节点（`nagi-agent` 服务）时拒绝安装带内嵌节点的面板；只装面板时需 `--no-agent` 并使用另一个数据目录。
- 带内嵌节点时确保 Docker 和 compose 插件可用，没有就自动安装；安装后把 nagi 注册为命令（`/usr/bin/nagi` 链接、bash 补全）。这些函数和节点安装脚本共用，源头是 `internal/server/api/install.go` 的 `sharedScript`，改动时两边一起改，`go test ./internal/server/api` 会检查。
- 没有 systemd 时只安装二进制，并打印需要自行运行的命令。

`scripts/uninstall.sh` 是面板和节点通用的卸载脚本。安装脚本或节点运行时新增了东西（服务、文件、容器标签、网络、镜像），要同步更新它：

- 默认：停止并删除 `nagi-server`、`nagi-agent` 服务，删除 `/usr/local/bin/nagi`、`/usr/bin/nagi` 链接和 bash 补全，删除代理容器（`nagi.link` 标签）和它们的镜像 `nagi-link:empty`。
- `--purge`：另外按 `nagi.project` 标签删除项目容器及其 compose 网络和 volume 对象、项目内网，以及服务配置里 `--data-dir` 指向的数据目录。
- 容器目录（含运行数据和快照）任何情况下都不删，只列出位置；Docker 和 `/etc/docker/daemon.json` 不动。

## 改为项目在面板、容器分节点

2026-10 改了项目模型（见 [README.md](README.md#核心概念)）：项目存在面板上，容器各自指定节点；同一项目的容器跨节点互通；项目之间单独连接；顶替、迁移、备份、快照按容器；只保留面板移交。当时还没有发布，不需要兼容旧数据和旧接口。按下面的顺序分步改，每一步都在界面上实际操作验证后再进行下一步：

1. **项目数据搬到面板**（已完成）：面板数据库加项目、容器、草稿、版本记录、部署记录、端口映射；草稿、部署、回滚、回到某次部署、删除改由面板指挥，节点只按下发的配置执行，并把配置缓存在 `node.db`。节点上去掉 `project.db` 和项目目录，改为容器目录；节点加"项目根目录"设置。这一步所有容器还在同一个节点上，跑通创建、部署、回滚、端口映射、上传、git、日志、终端、镜像更新。
2. **容器分节点**（已完成）：添加容器时选节点；项目内网的跨节点部分（代理名单由面板下发，代理连接时按"所在节点 → 顶替节点"查找）；去掉"关闭项目内网"。面板在每次节点上报后给各节点重算代理计划（`LinkPlan`，端口取节点上报的声明端口加项目设置的内部端口），变了才经 `SetLinks` 下发，节点存在 `links.pb`；候选节点目前只有所在节点，顶替节点在第 5 步加入。
3. **项目连接**（已完成）：网络名在面板上唯一；连接、断开两个项目；去掉节点上的共享网络和它的跨节点连接。连接存在面板的 `project_links` 表里，代理计划里为已连接项目的每个容器放"容器.网络名"的代理（对方在本节点上时直接转发）；连接、添加容器组、改网络名时检查撞名。
4. **按容器的快照、备份节点和迁移**（已完成）：每个容器一个 kopia 仓库；快照、恢复、同步、在备份节点上恢复、迁移、重新部署到其他节点都按容器；副本只带数据，配置由面板下发；去掉"副本特权配置"检查。备份节点写在下发给源节点的 `ContainerSpec.backup` 里，同步状态随容器状态上报；迁移、从备份节点恢复、重新部署到其他节点由面板按顺序指挥节点；原节点回来时，面板在它上报后让它停止已换到别处的容器（`DeleteContainer` 带 `retire`，目录改名保留）。界面在容器页的"数据"标签页。
5. **按容器的顶替和自动切回**（已完成）：租约按容器，同一对节点之间的续约合并发送（`MeshService.LeaseVotes`：发给同一个成员的投票等 30 毫秒一起发；续约时间对齐到 1/3 租约时长的整数倍，同样租约时长的容器在同一秒续约）。设置、取消、更换见证人、确认顶替在容器页的"数据"标签页；节点把参与的顶替状态随上报一起发给面板，面板据此更新容器的所在节点、补推热备的配置。
6. **面板丢失和移交**（已完成）：节点连上新面板时上报配置缓存，面板据此拼回项目；移交按整个项目检查节点，见证人先换成新面板；去掉共同管理。实现：面板发现节点上报了不知道的容器时调用 `GetCache` 拼回；移交改为一次提交一组节点（`HandoverNodes`），按项目相关的所有节点检查；新面板在节点上报时把见证人是别的面板的顶替换成自己；节点在新面板批准之前保留旧的 mesh 成员列表。
7. **界面**（已完成）：项目页的容器列表加"节点"列，添加容器选节点；容器页的"数据"标签页列出这个容器的快照、备份节点、顶替、迁移（最初放在项目的"数据"标签页，2026-10 界面改版时移到容器页）；"网络"标签页改为端口映射加项目连接；节点页去掉共享网络。最后在测试环境里按真实用户把跨节点通信、顶替、自动切回、迁移、面板丢失后恢复都走一遍。另外加了：移交时按项目勾选（自动带上相关节点）、节点页的"副本"标签（列出本节点保存的副本，没有容器使用的可以删除）。

## 主备面板

2026-10 加主备面板（见 [standby.md](standby.md)）。分步做，每一步在界面上实际操作验证：

1. **配对和同步**（已完成）：主面板生成配对码，`nagi server --standby <配对码>` 配对（主密钥、CA、数据库），主面板变化后几秒内把数据库快照发给备用面板，保留各自的本地设置和会话；设置页列出备用面板和同步状态，可以移除。
2. **节点连接备用面板和任期**（已完成）：主面板把备用面板加进节点的信任列表；面板连上节点时带上任期和主备身份，节点只执行任期最大的主面板发来的修改；备用面板的接口拒绝修改，界面只读。
3. **提升和降级**（已完成）：备用面板手动提升（任期加一，通知节点，其他备用面板改为从它同步，见证人换成它）；旧主面板回来发现任期落后自动降为备用。最后在测试环境里走一遍：配对、主面板改东西备用面板看到、停掉主面板、提升、继续修改和部署、旧主面板回来降为备用。

## 第 1 步的完成标准

**目标：单机跑通一条完整链路。** 每一层都有最简实现，之后每一步往里填。

1. `nagi server` 启动：随机选面板端口并持久化，生成自签 CA，日志里打印带一次性令牌的初始化链接。
2. 网页上完成初始化，设置管理员账号，用密码登录（含登录限速）。
3. 内嵌 agent 在本机自动加入：走 mTLS + yamux，反向 gRPC 打通，面板上能看到这个节点在线。
4. 在网页上新建项目：填项目名、项目目录，粘贴一份 compose。agent 生成项目 ID，创建项目目录，解析 compose 并生成 override（本地网段、标签、日志轮转、重启策略），记录第一个版本。
5. 点部署：agent 调用 `docker compose up`，面板上能看到每个服务的容器状态，并实时查看日志。

**这一步不做**：Caddy 出口（第 1 步暂时保留 compose 里的 `ports`，第 2 步接入 Caddy 后再由 override 去掉）、草稿、镜像表单、容器的环境变量、文件来源、终端、TOTP、快照、远程节点、mesh。修订号从这一步起就要有。
