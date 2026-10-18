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
│   ├── sealer/       面板主密钥加密
│   └── store/        面板数据库（sqlc）
└── agent/            节点：连接面板、信任列表、mesh、副本同步、顶替、跨节点共享网络
    ├── project/      项目和成员、网络、版本与回滚、快照、端口映射、override 生成、重建
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

nagi 要静态编译（`CGO_ENABLED=0`）：跨节点共享网络的代理容器用空镜像，运行的是挂进去的 nagi 本身。内嵌 Caddy 后二进制约 90MB；发布构建时加 `-ldflags "-s -w"` 去掉调试信息可以小不少。

前端开发时可以用 `pnpm -C web dev`，它会把接口请求转发到 `https://127.0.0.1:28443`（可用 `NAGI_PANEL` 环境变量修改），浏览器打开 `http://localhost:5173`。

**当前前端只是用来测试后端的临时界面，之后会重写。** 重写时别漏了各文档里写到的这些提示（临时界面里已经有了）：

- 加入共享网络时网络名默认由项目名生成；改网络名时提示连它的项目要跟着改（[agent.md](agent.md#共享网络)）
- 设置顶替、迁移、用配置备份重建时，提示搬过去以后会断开哪些共享网络的连接（[failover.md](failover.md#实现)）
- 不在快照范围内的 volume（`external` 或自定义 driver，接口里是成员的 `unmanaged_volumes`）和 `network_mode: host` 的服务（`host_network_services`）醒目提示（[agent.md](agent.md#部署方式)）
- 首页提醒开启两步验证；提醒单独备份面板主密钥文件（设置里的 `master_key_path`，见 [panel.md](panel.md)）

## 第 1 步的完成标准

**目标：单机跑通一条完整链路。** 每一层都有最简实现，之后每一步往里填。

1. `nagi server` 启动：随机选面板端口并持久化，生成自签 CA，日志里打印带一次性令牌的初始化链接。
2. 网页上完成初始化，设置管理员账号，用密码登录（含登录限速）。
3. 内嵌 agent 在本机自动加入：走 mTLS + yamux，反向 gRPC 打通，面板上能看到这个节点在线。
4. 在网页上新建项目：填项目名、项目目录，粘贴一份 compose。agent 生成项目 ID，创建项目目录，解析 compose 并生成 override（本地网段、标签、日志轮转、重启策略），记录第一个版本。
5. 点部署：agent 调用 `docker compose up`，面板上能看到每个服务的容器状态，并实时查看日志。

**这一步不做**：Caddy 出口（第 1 步暂时保留 compose 里的 `ports`，第 2 步接入 Caddy 后再由 override 去掉）、草稿、镜像表单、服务级环境变量、文件来源、终端、TOTP、快照、远程节点、mesh。修订号从这一步起就要有。
