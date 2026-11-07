<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/hero.dark.png">
    <img src="docs/assets/hero.png" alt="Nagi" width="860">
  </picture>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="License: Apache-2.0"></a>
  <img src="https://img.shields.io/badge/platform-Linux%20amd64%20%7C%20arm64-lightgrey" alt="Platform: Linux amd64 | arm64">
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" alt="Go 1.26">
  <a href="https://github.com/SIPC/Nagi/releases/latest"><img src="https://img.shields.io/github/v/release/SIPC/Nagi" alt="Release"></a>
</p>

<p align="center"><b>简体中文</b> · <a href="README.en.md">English</a></p>

# Nagi（凪）

> 风平浪静。

Nagi 是一个开源的自托管容器部署平台，可以在一个网页面板上管理分布在多台 Linux 服务器上的 Docker 容器。Nagi 基于 Docker 和 Docker Compose，支持多节点与高可用。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/screenshots/overview.dark.png">
  <img src="docs/assets/screenshots/overview.png" alt="概览：节点状态和实时流量">
</picture>

## 工作方式

- **基于 Docker Compose**：每个应用都是一份标准的 compose 文件，加上环境变量和密钥；用镜像表单创建的单个容器也自动生成 compose 文件。部署、版本和回滚都以 compose 文件为单位，Nagi 不改写已有的 compose 文件。
- **一个面板，多个节点**：面板是网页控制台，保存项目的配置；节点是运行容器的服务器，主动连接面板，不需要开放入站端口。面板默认内嵌一个节点，一台服务器即可开始使用。
- **节点网络**：节点之间通过 WireGuard 加密连接。同一项目的容器无论在哪台服务器上，都可以按名称互相访问。
- **对外访问**：内置 Caddy，绑定域名后自动申请 HTTPS 证书，也可以直接开放 TCP 和 UDP 端口。
- **数据保护**：基于 kopia 按容器定时快照，可以同步到另一个节点作为异地副本。节点故障时，容器自动顶替到事先指定的顶替节点。
- **单个程序**：面板和节点是同一个用 Go 编写的程序，数据保存在内置的 SQLite 中，除 Docker 外不依赖其他服务。

### 核心概念

| 概念 | 含义 |
| --- | --- |
| 节点 | 一台运行 Nagi 节点程序的 Linux 服务器，容器运行在节点上 |
| 项目 | 一组一起工作的容器，例如前端、后端和数据库。项目存在面板上，里面的容器可以分布在不同节点上；项目之间默认隔离 |
| 容器 | 项目里独立部署的单位：单个容器，或一份 compose 定义的容器组。各自部署、回滚、快照和迁移 |

## 界面

**项目**：一组一起工作的容器，可以分布在不同节点上，统一部署和监控。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/screenshots/project.dark.png">
  <img src="docs/assets/screenshots/project.png" alt="项目页：容器列表和监控">
</picture>

**容器**：每个容器有单独的页面，显示状态、资源用量、版本和最近事件。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/screenshots/container.dark.png">
  <img src="docs/assets/screenshots/container.png" alt="容器页：镜像、节点、版本和资源用量">
</picture>

**节点**：显示每台服务器的 CPU、内存和磁盘用量。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/screenshots/nodes.dark.png">
  <img src="docs/assets/screenshots/nodes.png" alt="节点页：各节点的资源用量">
</picture>

**节点网络**：显示节点之间的连接方式和延迟，包括直连和经面板中转的连接。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/screenshots/network.dark.png">
  <img src="docs/assets/screenshots/network.png" alt="节点网络：节点之间的连接和延迟">
</picture>

## 特性

### 多节点与高可用

![一个面板，管理多个节点](docs/assets/architecture.png)

![跨节点，按名字访问](docs/assets/intranet.png)

![跨节点入口，一个容器多个地址](docs/assets/entry.png)

![节点故障，自动接替](docs/assets/failover.png)

![面板故障，服务不受影响](docs/assets/recovery.png)

此外还支持：

- 快照：按容器定时备份与恢复，可以指定备份节点作为异地副本；容器可以在节点之间迁移，数据随容器迁移；节点无法恢复时，可以将容器重新部署到其他节点
- 主备面板，以及在面板之间移交节点
- 节点终端与文件：不需要 SSH，仅管理员可用，操作前验证身份并记录在活动中；可以在节点上关闭

### 基础功能

![一个项目，一组容器](docs/assets/model.png)

![每次部署，都是一个版本](docs/assets/versions.png)

![统一入口，自动 HTTPS](docs/assets/exposure.png)

此外还支持：

- 日志、终端、监控与告警（webhook、Telegram），面板离线时告警照常发送
- 环境变量（项目、容器组、容器三层）与密钥、私有镜像仓库认证、健康检查、资源限制
- 镜像更新提醒、国内节点镜像加速
- 项目之间默认隔离，需要时可以连接两个项目
- 面板安全：两步验证、登录限速与安全入口；在面板上一键更新面板和升级节点

### 暂不支持

自动调度、单个容器多副本运行、多面板同时管理、恢复演练、接管已有项目、在节点上构建、应用模板。跨节点的容器之间只转发 TCP 和单播 UDP。

## 快速开始

在 Linux 服务器上以 root 身份执行以下命令，安装面板。面板内嵌一个节点，一台服务器即可开始使用。

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh
```

安装完成后，在浏览器中打开终端输出的初始化链接，设置管理员账号。

详细步骤见 [使用教程](docs/guide/README.md)：

- [安装面板](docs/guide/install.md)
- [添加节点](docs/guide/add-node.md)
- [添加备用面板](docs/guide/standby.md)
- [升级与卸载](docs/guide/upgrade.md)
- [更换面板](docs/guide/migrate-panel.md)

## 系统要求

- Linux（amd64 / arm64），建议使用 systemd
- Docker 与 compose 插件 2.24 或更高版本（安装脚本自动安装）
- 以下端口需要放行：

| 端口 | 协议 | 用途 |
| --- | --- | --- |
| 面板端口 | TCP、UDP | 网页控制台与节点连接，须能被浏览器和所有节点访问；UDP 用于 WireGuard 中转 |
| 80、443 | TCP | 项目域名访问与证书申请，仅在通过域名对外提供服务的节点上需要 |
| 节点的 WireGuard 端口 | UDP | 节点之间直连，首次启动时随机选择；未放行时自动经面板中转 |

在面板上为容器开放的 TCP 和 UDP 端口，也需要在对应节点上放行。

## 参与开发

各模块的设计见 [设计文档](docs/README.md)，从源码构建与本地开发见 [开发规划](docs/development.md#本地开发)。

## 安全

不要通过公开的 issue 报告安全问题。报告方式见 [SECURITY.md](SECURITY.md)。

## 致谢

Nagi 构建在以下开源项目之上：

- [Caddy](https://github.com/caddyserver/caddy) 与 [caddy-l4](https://github.com/mholt/caddy-l4)：对外访问、自动 HTTPS 与 TCP / UDP 转发
- [wireguard-go](https://git.zx2c4.com/wireguard-go)：节点之间的加密网络
- [Hysteria](https://github.com/apernet/hysteria) 与 [quic-go](https://github.com/quic-go/quic-go)：QUIC 快速通道与 BBR 拥塞控制
- [kopia](https://github.com/kopia/kopia)：快照与异地副本
- [yamux](https://github.com/hashicorp/yamux)、[Connect](https://connectrpc.com)：控制面通信
- [shadcn/ui](https://ui.shadcn.com)：界面组件
- [MiSans](https://hyperos.mi.com/font)：界面字体，由小米提供

## 许可证

[Apache-2.0](LICENSE)
