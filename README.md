<p align="center"><img src="docs/assets/hero.png" alt="Nagi · 风平浪静" width="860"></p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="License: Apache-2.0"></a>
  <img src="https://img.shields.io/badge/platform-Linux%20amd64%20%7C%20arm64-lightgrey" alt="Platform: Linux amd64 | arm64">
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" alt="Go 1.26">
  <a href="https://github.com/SIPC/Nagi/releases/latest"><img src="https://img.shields.io/github/v/release/SIPC/Nagi" alt="Release"></a>
</p>

<p align="center"><b>简体中文</b> · <a href="README.en.md">English</a></p>

# Nagi（凪）

> 风平浪静。

Nagi 是一个开源的自托管容器部署平台：在一个网页面板上，管理分布在多台 Linux 服务器上的 Docker 容器。它基于 Docker 和 Docker Compose，支持多节点与高可用，追求简单、美观、易于部署，可直接用于生产环境。

## 工作方式

- **基于 Docker Compose**：每个应用都是一份标准的 compose 文件，加上环境变量和密钥；用镜像表单创建的单个容器也会自动生成 compose。部署、版本、回滚都以 compose 为单位，Nagi 不改写你的 compose 文件。
- **一个面板，多个节点**：面板是网页控制台，保存项目的配置；节点是运行容器的服务器，主动连接面板，无需开放入站端口。面板默认内嵌一个节点，一台服务器即可使用。
- **节点之间自动组网**：节点之间通过 WireGuard 加密互联，同一项目的容器不论在哪台服务器上，都能按名字互相访问。
- **对外访问**：内置 Caddy，绑定域名后自动申请 HTTPS 证书，也可以直接开放 TCP / UDP 端口。
- **数据保护**：基于 kopia 按容器定时快照，可同步到另一个节点作为异地副本；节点故障时，可由事先指定的节点自动接替。
- **单个程序**：用 Go 编写，面板和节点是同一个程序，数据存在内置的 SQLite 中，除 Docker 外不依赖其他服务。

### 核心概念

| 概念 | 含义 |
| --- | --- |
| 节点 | 一台运行 Nagi 节点程序的 Linux 服务器，容器运行在节点上 |
| 项目 | 一组一起工作的容器，例如前端、后端和数据库。项目存在面板上，里面的容器可以分布在不同节点上；项目之间默认隔离 |
| 容器 | 项目里独立部署的单位：单个容器，或一份 compose 定义的容器组。各自部署、回滚、快照和迁移 |

## 特性

### 多节点与高可用

![一个面板，管理多个节点](docs/assets/architecture.png)

![跨节点，按名字访问](docs/assets/intranet.png)

![跨节点入口，一个容器多个地址](docs/assets/entry.png)

![节点故障，自动接替](docs/assets/failover.png)

![面板故障，服务不受影响](docs/assets/recovery.png)

此外还支持：

- 快照：按容器定时备份与恢复，可指定备份节点作为异地副本；容器可在节点间迁移，数据不丢失；节点彻底损坏时，可将容器重新部署到其他节点
- 主备面板，以及在面板之间移交节点
- 节点终端与文件：无需 SSH，仅管理员可用，操作前验证身份，全程记录；可在节点上关闭

### 基础功能

![一个项目，一组容器](docs/assets/model.png)

![每次部署，都是一个版本](docs/assets/versions.png)

![统一入口，自动 HTTPS](docs/assets/exposure.png)

此外还支持：

- 日志、终端、监控与告警（webhook、Telegram），面板离线时告警照常发送
- 环境变量（项目、容器组、容器三层）与密钥、私有镜像仓库认证、健康检查、资源限制
- 镜像更新提醒、国内节点镜像加速
- 项目之间默认隔离，可按需连接两个项目
- 面板安全：可选两步验证、登录限速与安全入口；可在面板上一键更新面板和升级节点

### 暂不支持

自动调度、单个容器多副本运行、多面板同时管理、恢复演练、接管已有项目、在节点上构建、应用模板。跨节点的容器之间只转发 TCP 和单播 UDP。

## 快速开始

### 安装面板

在 Linux 服务器上以 root 执行：

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh
```

脚本会下载 nagi 并校验 SHA-256，以 systemd 服务运行面板；面板默认内嵌一个节点，单机即可使用，未安装 Docker 时会自动安装。完成后终端会输出面板地址、公钥指纹和初始化链接，在浏览器中打开链接即可设置管理员账号。面板使用自签证书，浏览器提示不安全时，核对公钥指纹后继续即可。

常用选项（追加在命令末尾，例如 `| sudo sh -s -- --port 28443`）：

| 选项 | 说明 |
| --- | --- |
| `--port <端口>` | 指定面板端口，默认首次启动时随机选择 |
| `--no-agent` | 只运行面板，不内嵌节点，适合把面板单独放在一台机器上 |
| `--docker-mirror <名称>` | 自动安装 Docker 时使用的软件源镜像：`Aliyun` 或 `AzureChinaCloud`，适用于国内服务器 |
| `--download-url <地址>` | 从镜像或内网地址下载 nagi，适用于无法访问 GitHub 的环境；之后一键更新面板也从该地址下载 |
| `--version <版本>` | 安装指定版本，默认为最新版本 |

### 添加节点

在面板的「节点」页复制安装命令，在新服务器上以 root 执行。脚本从 GitHub Release 下载与面板相同版本的 nagi 并校验 SHA-256，节点同样会自动安装 Docker。无法访问 GitHub 时，在命令末尾加上 `--download-url <镜像地址>`，之后在面板上一键升级节点也会从该地址下载。

![一行命令，加入新节点](docs/assets/join.png)

### 添加备用面板

在主面板「设置 → 备用面板」中生成配对码，在新服务器上执行：

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh -s -- --standby <配对码>
```

主面板故障时，可在备用面板上将其提升为主面板。

### 升级

在面板的「设置 → 版本更新」一键更新面板，也可以再次执行安装脚本，端口、数据目录等配置保持不变；节点可在面板的「节点」页一键升级。

### 卸载

面板和节点通用：

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/uninstall.sh | sudo sh
```

默认只卸载程序和服务，项目容器与 Nagi 数据目录保留；加 `-s -- --purge` 会同时删除 Nagi 管理的容器、网络和数据目录。项目文件（容器目录，默认在 `/srv/nagi` 下）不会被删除，脚本执行完会列出它们的位置。Docker 本身不卸载。卸载节点前，建议先在面板上迁移或删除它的容器。

### 更换面板

在新面板上生成接入码，粘贴到旧面板即可移交节点，新面板根据节点缓存自动重建项目（草稿与历史版本记录不会保留）。若旧面板已不可用，可在节点上执行：

```bash
nagi panel add <接入码>
nagi panel remove <旧面板地址>
```

## 系统要求

- Linux（amd64 / arm64），建议使用 systemd
- Docker 与 compose 插件 2.24 及以上版本（安装脚本会自动安装）
- 需要放行的端口：

| 端口 | 协议 | 用途 |
| --- | --- | --- |
| 面板端口 | TCP、UDP | 网页控制台与节点连接，需能被浏览器和所有节点访问；UDP 用于 WireGuard 中转 |
| 80、443 | TCP | 项目域名访问与证书申请，仅在通过域名对外提供服务的节点上需要 |
| 节点的 WireGuard 端口 | UDP | 节点之间直连，首次启动时随机选择；未放行时自动经面板中转 |

此外，在界面上为容器开放的 TCP / UDP 端口也需要在对应节点上放行。

## 架构

只有一个二进制 `nagi`：`nagi server` 为面板，`nagi agent` 为节点。

- **控制面**：节点以 mTLS 主动连接面板，无需开放入站端口；单条连接经 yamux 多路复用，面板通过反向 gRPC 调用节点。面板使用自签 CA，节点以公钥指纹校验面板身份。
- **数据面**：节点之间基于用户态 wireguard-go 组成全互联网络，地址由公钥派生；直连不可达时依次回退至面板 UDP 中继与 TLS 中继。
- **授权**：网络连通不代表有权访问，每次调用均按调用方公钥单独鉴权。

各模块的详细设计见 [设计文档](docs/README.md)。

## 参与开发

- 后端：Go、gRPC、Connect-RPC、SQLite（modernc）
- 前端：Vite、React、TypeScript、TanStack Router / Query、shadcn/ui

从源码构建与本地开发见 [开发规划](docs/development.md#本地开发)。

## 安全

请勿通过公开的 issue 报告安全问题，报告方式见 [SECURITY.md](SECURITY.md)。

## 致谢

Nagi 构建在以下开源项目之上：

- [Caddy](https://github.com/caddyserver/caddy) 与 [caddy-l4](https://github.com/mholt/caddy-l4)：对外访问、自动 HTTPS 与 TCP / UDP 转发
- [wireguard-go](https://git.zx2c4.com/wireguard-go)：节点之间的加密网络
- [kopia](https://github.com/kopia/kopia)：快照与异地副本
- [yamux](https://github.com/hashicorp/yamux)、[Connect](https://connectrpc.com)：控制面通信
- [shadcn/ui](https://ui.shadcn.com)：界面组件
- [MiSans](https://hyperos.mi.com/font)：界面字体，由小米提供

## 许可证

[Apache-2.0](LICENSE)
