<p align="center"><img src="docs/assets/hero.png" alt="Nagi · 风平浪静" width="860"></p>

# Nagi（凪）

> 风平浪静。

Nagi 是一个开源的 Docker 项目面板，追求简单、美观、易于部署，可直接用于生产环境，并支持多节点与高可用。

> 状态：开发中，尚未发布。

## 特性

![一个项目，一组容器](docs/assets/model.png)

![每次部署，都是一个版本](docs/assets/versions.png)

![统一入口，自动 HTTPS](docs/assets/exposure.png)

![一个面板，管理多个节点](docs/assets/architecture.png)

![跨节点，按名字访问](docs/assets/intranet.png)

![节点故障，自动接替](docs/assets/failover.png)

![面板故障，服务不受影响](docs/assets/recovery.png)

此外还支持：

- 日志、终端、监控与告警（webhook、Telegram），面板离线时告警照常发送
- 快照：按容器定时备份与恢复，可指定备份节点作为异地副本；容器可在节点间迁移，数据不丢失；节点彻底损坏时，可将容器重新部署到其他节点
- 环境变量（项目、容器组、容器三层）与密钥、私有镜像仓库认证、健康检查、资源限制
- 镜像更新提醒、国内节点镜像加速
- 项目之间默认隔离，可按需连接两个项目
- 主备面板，以及在面板之间移交节点
- 面板安全：可选两步验证、登录限速与安全入口；可在面板上一键升级节点

暂不支持：自动调度、单个容器多副本运行、多面板同时管理、多用户、恢复演练、接管已有项目、在节点上构建、应用模板。跨节点的容器之间仅转发 TCP。

## 快速开始

> 尚未发布，以下命令可能变更。

支持 Linux（amd64 / arm64），前置依赖为 Docker 与 compose 插件（2.24 及以上版本）。

**安装面板**：用 root 执行。脚本下载 nagi 并校验 SHA-256，安装为 systemd 服务 `nagi-server`，启动后打印面板地址和初始化链接。面板内嵌一个节点，单机即可使用。首次启动时随机选择一个端口，也可通过 `--port` 指定；80 与 443 端口保留给项目域名。

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sh
```

可选参数（追加在 `sh -s --` 之后）：`--port <端口>`、`--no-agent`（只运行面板，不带内嵌节点）、`--version <版本>`、`--download-url <镜像地址>`，完整说明见 `--help`。再次执行即为升级。

**添加节点**：在面板中复制安装命令，在新服务器上执行。

```bash
curl --pinnedpubkey "sha256//<面板公钥指纹>" -k https://<地址:端口>/install.sh | sh -s -- --token <令牌>
```

![一行命令，加入新节点](docs/assets/join.png)

**添加备用面板**：在主面板设置页生成配对码，在新服务器上用该配对码安装。主面板故障时，可在备用面板上将其提升为主面板。

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sh -s -- --standby <配对码>
```

**更换面板**：在新面板上生成接入码，粘贴到旧面板即可移交节点，新面板根据节点缓存自动重建项目（草稿与历史版本记录不会保留）。若旧面板已不可用，可在节点上执行：

```bash
nagi panel add <接入码>
nagi panel remove <旧面板地址>
```

## 架构

只有一个二进制 `nagi`：`nagi server` 为面板，`nagi agent` 为节点。

- **控制面**：节点以 mTLS 主动连接面板，无需开放入站端口；单条连接经 yamux 多路复用，面板通过反向 gRPC 调用节点。面板使用自签 CA，节点以公钥指纹校验面板身份。
- **数据面**：节点之间基于用户态 wireguard-go 组成全互联网络，地址由公钥派生；直连不可达时依次回退至面板 UDP 中继与 TLS 中继。
- **授权**：网络连通不代表有权访问，每次调用均按调用方公钥单独鉴权。

各模块的详细设计见 [设计文档](docs/README.md)。

## 技术栈

- 后端：Go、gRPC、Connect-RPC、SQLite（modernc）、wireguard-go、内嵌 Caddy（含 layer4）、内嵌 kopia
- 前端：Vite、React、TypeScript、TanStack Router / Query、shadcn/ui

## 许可证

Apache-2.0
