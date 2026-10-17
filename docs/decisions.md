# 技术决策

各模块文档中的关键决策汇总在这里，方便查阅理由。新增决策时同时更新对应模块的文档。

## 架构

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 数据归属 | **项目数据以节点为准，面板只存配置备份** | 面板可随时替换、可多实例，不需要 Raft | [failover.md](failover.md#面板故障) |
| 项目标识 | **全局唯一的项目 ID，项目名只用于显示** | 迁移、顶替、多面板时不会因重名冲突 | [agent.md](agent.md#项目标识) |
| 修改与部署 | **先存草稿，点部署才生效并生成版本** | 可以改好几处再一起上线，不会反复重启 | [agent.md](agent.md#修改与部署) |
| 删除项目 | **删除容器，项目目录默认保留** | 没说要动的就不动；删目录需输入项目名确认 | [agent.md](agent.md#删除项目) |

## 项目与部署

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 创建方式 | **写 compose，或填镜像表单（文件可选）** | 只有两种入口，底层都是 compose | [agent.md](agent.md#创建方式) |
| 环境变量 | **项目级（替换 `${变量}`）+ 服务级（注入容器）** | 与 compose 语义一致；密码只进需要它的容器 | [agent.md](agent.md#环境变量) |
| 镜像更新 | **每天检查并提醒，手动更新；版本记录镜像摘要** | 不会半夜自动升级导致业务挂掉；回滚能回到当时那份镜像 | [agent.md](agent.md#镜像) |
| 镜像加速 | **按节点配置，用户配置后才写入 Docker 配置** | 国内外节点需求不同；不擅自改 Docker | [agent.md](agent.md#镜像) |
| host 网络 | **允许，界面醒目提示** | 部分应用只能用 host 网络 | [agent.md](agent.md#部署方式) |
| 对外访问 | **全部经 Caddy（含 layer4），容器不映射端口** | 出口统一，不会绕过 ufw；以后切换流量只改 Caddy | [exposure.md](exposure.md) |

## 通信与网络

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 控制面协议 | **gRPC**（跑在 mTLS 上） | 双向流和多路复用是现成的；proto 自带版本演进，新功能不容易弄坏旧 agent | [control-plane.md](control-plane.md) |
| 面板调用 agent | **反向 gRPC，底层 yamux 复用一条 TLS 连接** | 每个操作都是独立的强类型接口，日志、终端是普通流式接口，加功能只需加方法 | [control-plane.md](control-plane.md#接口设计) |
| 浏览器接口 | **Connect-RPC，终端用 WebSocket** | 和节点共用 proto，自动生成 TypeScript 类型；支持普通 JSON，可用 curl 调用 | [control-plane.md](control-plane.md#接口设计) |
| 版本兼容 | **面板兼容旧一个大版本的节点，可一键升级节点** | 升级时不用所有机器同时停；proto 只加字段不改删 | [control-plane.md](control-plane.md#接口设计) |
| WireGuard 实现 | **全部用用户态 wireguard-go，自己实现直连 / UDP 中转 / TLS 中转** | 一套代码路径；内核版做不到套 TLS 中转 | [mesh.md](mesh.md#实现全部使用用户态-wireguard-go) |
| mesh 地址 | **由 WireGuard 公钥推导的 IPv6** | 不用分配，mesh 合并不冲突，无法冒用他人地址 | [mesh.md](mesh.md#地址) |
| 容器网段 | **节点本地分配，默认 `10.200.0.0/16`，每项目 /26** | 不需要全局协调 | [mesh.md](mesh.md#容器网段只在节点内分配) |
| 租约投票通道 | **全部走 WireGuard mesh，面板加入 mesh** | 投票只有一套代码；面板在 mesh 里也负责中转项目文件上传 | [mesh.md](mesh.md) |

## 高可用与备份

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 租约时长 | **默认 30 秒，每 10 秒续约一次，可按项目调整** | A 挂掉后大约 40 秒内 B 顶上 | [failover.md](failover.md) |
| 租约安全余量 | **A 在到期前 5 秒已停完，B 在到期后 5 秒启动** | 中间留 10 秒空档，即使两边计时有误差也不会同时运行 | [failover.md](failover.md) |
| 快照工具 | **kopia，作为 Go 库内嵌** | 增量、去重、自带加密，以后接 S3 是现成的；仍然只有一个二进制 | [snapshot.md](snapshot.md) |
| 快照一致性 | **暂停快照，30 秒上限；导出方式作为高级选项（需同时填导入命令）** | 恢复和顶替只是拷文件，快且通用 | [snapshot.md](snapshot.md#一致性) |
| 快照同步频率 | **设了顶替的项目每 15 分钟，其余每小时** | kopia 是增量的，频率高也不会占用太多资源 | [snapshot.md](snapshot.md#频率) |
| 异地副本 | **顶替节点，或只同步不顶替的备份节点** | 复用同步代码；S3 以后再加 | [snapshot.md](snapshot.md#异地副本) |
| 面板上的密钥 | **面板主密钥加密** | 数据库文件泄露也读不出密钥；需提醒用户单独备份主密钥文件 | [panel.md](panel.md#配置备份) |
| 监控数据 | **存在节点本地，面板按需查询** | 多面板不用同步，面板不会变重 | [monitoring.md](monitoring.md) |

## 开发

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 运行平台 | **Linux（amd64 / arm64），systemd** | Caddy 直接访问容器 IP、WireGuard 等都依赖 Linux | [development.md](development.md) |
| Docker 交互 | **部署用 compose 命令，其余用 Docker Engine API** | 部署行为和用户手动操作完全一致；状态、日志、监控需要结构化数据 | [development.md](development.md#依赖库) |
| 数据库访问 | **sqlc** | 写 SQL 生成强类型代码，没有运行时魔法 | [development.md](development.md#依赖库) |
| 前端 | **Vite + React + TS + TanStack Router/Query + shadcn/ui** | 纯静态输出可打包进二进制；路由类型安全 | [development.md](development.md#依赖库) |
| 许可证 | **Apache-2.0** | 宽松，允许商用，带专利授权条款 | [development.md](development.md) |

## 待定问题

暂无。
