# 技术决策

各模块文档中的关键决策汇总在这里，方便查阅理由。新增决策时同时更新对应模块的文档。

## 架构

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 数据归属 | **项目数据以节点为准，面板只存配置备份** | 面板可随时替换、可多实例，不需要 Raft | [failover.md](failover.md#面板故障) |
| 项目标识 | **全局唯一的项目 ID，项目名只用于显示** | 迁移、顶替、多面板时不会因重名冲突 | [agent.md](agent.md#项目标识) |
| 项目结构 | **项目 = 一组成员；成员是单容器或一份 compose，各自独立部署、记版本、回滚** | 前端、后端、数据库一起管理，但改一个不碰别的；迁移、顶替、备份按整个项目，互相依赖的东西一起搬 | [agent.md](agent.md#项目与成员) |
| 项目内网 | **默认开启，成员用"成员名"或"服务.成员"互相访问；项目之间默认隔离，可加入同一节点上的共享网络** | 开箱即用，又不会因为两份 compose 都有 db 而冲突 | [agent.md](agent.md#网络) |
| 接入内网 | **compose 成员由节点用 `network connect --alias` 接入，不写进 override** | compose 会把服务原名注册到它加入的每个网络上，写进 override 就会撞名 | [agent.md](agent.md#网络) |
| 共享网络里的名字 | **成员.网络名，网络名由项目单独设置、本节点唯一** | 项目名可以重名、可以改，直接用会撞、会变 | [agent.md](agent.md#共享网络) |
| 导入多服务 compose | **先只支持整份作为一个 compose 成员** | 拆开要处理 depends_on、共用 volume、共用 .env 等，容易出错；自己的系统可以一个个成员地加 | [agent.md](agent.md#创建方式) |
| 一起部署 | **可以一次部署多个成员，按成员设置的"先部署的成员"排序；每次部署在项目上记一条部署记录，可以整个项目回到某次部署时的状态** | 前后端一起改的东西要一起上线、一起回滚 | [agent.md](agent.md#版本与回滚) |
| 变量 | **项目共享变量 + 成员变量 + 服务变量，同名以更具体的为准；共享变量也走草稿，成员版本记下当时用的值** | 数据库密码之类的不用在多个成员里各填一遍；自动部署不会悄悄带上没部署的改动，回滚能回到当时的值 | [agent.md](agent.md#环境变量) |
| 修订号 | **按成员，项目设置另有一个** | 两个面板同时改不同的成员不会误报冲突 | [agent.md](agent.md#修改与部署) |
| 修改与部署 | **先存草稿，点部署才生效并生成版本** | 可以改好几处再一起上线，不会反复重启 | [agent.md](agent.md#修改与部署) |
| 删除项目 | **删除容器，项目目录默认保留** | 没说要动的就不动；删目录需输入项目名确认 | [agent.md](agent.md#删除) |
| 格式版本号 | **项目目录和副本都带格式版本号，看不懂就拒绝** | 项目会在不同版本的节点之间搬，以后改目录结构不会被旧节点用错 | [agent.md](agent.md#项目目录) |

## 项目与部署

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 创建方式 | **写 compose，或填镜像表单（文件可选）** | 只有两种入口，底层都是 compose | [agent.md](agent.md#创建方式) |
| 镜像更新 | **每天检查并提醒，手动更新；版本记录镜像摘要** | 不会半夜自动升级导致业务挂掉；回滚能回到当时那份镜像 | [agent.md](agent.md#镜像) |
| 镜像加速 | **按节点配置，用户配置后才写入 Docker 配置** | 国内外节点需求不同；不擅自改 Docker | [agent.md](agent.md#镜像) |
| host 网络 | **允许，界面醒目提示** | 部分应用只能用 host 网络 | [agent.md](agent.md#部署方式) |
| 对外访问 | **全部经 Caddy（含 layer4），容器不映射端口** | 出口统一，不会绕过 ufw；以后切换流量只改 Caddy | [exposure.md](exposure.md) |
| 端口映射 | **项目目录 config/routes/ 每条一个文件，立即生效；部署时只导入 compose 新增的端口** | 有些镜像要手动加映射；映射不动容器，不必重新部署；用户改过或删掉的不会被覆盖 | [exposure.md](exposure.md#端口映射目录) |

## 通信与网络

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 控制面协议 | **gRPC**（跑在 mTLS 上） | 双向流和多路复用是现成的；proto 自带版本演进，新功能不容易弄坏旧 agent | [control-plane.md](control-plane.md) |
| 面板调用 agent | **反向 gRPC，底层 yamux 复用一条 TLS 连接** | 每个操作都是独立的强类型接口，日志、终端是普通流式接口，加功能只需加方法 | [control-plane.md](control-plane.md#接口设计) |
| 浏览器接口 | **Connect-RPC，终端用 WebSocket** | 和节点共用 proto，自动生成 TypeScript 类型；支持普通 JSON，可用 curl 调用 | [control-plane.md](control-plane.md#接口设计) |
| 版本兼容 | **面板兼容旧一个大版本的节点，可一键升级节点** | 升级时不用所有机器同时停；proto 只加字段不改删 | [control-plane.md](control-plane.md#接口设计) |
| WireGuard 实现 | **全部用用户态 wireguard-go，自己实现直连 / UDP 中转 / TLS 中转** | 一套代码路径；内核版做不到套 TLS 中转 | [mesh.md](mesh.md#实现全部使用用户态-wireguard-go) |
| mesh 地址 | **由 WireGuard 公钥推导的 IPv6** | 不用分配，mesh 合并不冲突，无法冒用他人地址 | [mesh.md](mesh.md#地址) |
| 容器网段 | **节点本地分配，默认 `10.200.0.0/16`，项目内网、compose 成员、共享网络各一个 /26** | 不需要全局协调 | [mesh.md](mesh.md#容器网段只在节点内分配) |
| 租约投票通道 | **全部走 WireGuard mesh，面板加入 mesh** | 投票只有一套代码；面板在 mesh 里也负责中转项目文件上传 | [mesh.md](mesh.md) |

## 高可用与备份

| 问题 | 决定 | 理由 | 详见 |
| --- | --- | --- | --- |
| 租约时长 | **默认 30 秒，每 10 秒续约一次，可按项目调整** | A 挂掉后大约 40 秒内 B 顶上 | [failover.md](failover.md) |
| 顶替三方的连通 | **移除面板前检查：移除后顶替的参与方之间还要能经别的面板连上；见证人可以直接更换** | 投票走 mesh，mesh 只连通同一个面板下的成员；不检查的话顶替会悄悄失效 | [failover.md](failover.md#租约与三方多数票) |
| 租约安全余量 | **A 在到期前 5 秒已停完，B 在到期后 5 秒启动** | 中间留 10 秒空档，即使两边计时有误差也不会同时运行 | [failover.md](failover.md) |
| 快照工具 | **kopia，作为 Go 库内嵌** | 增量、去重、自带加密，以后接 S3 是现成的；仍然只有一个二进制 | [snapshot.md](snapshot.md) |
| 快照一致性 | **暂停快照，30 秒上限；不针对具体数据库做导出导入** | 恢复、迁移和顶替都只是拷文件，快且通用，不用为每种数据库做适配 | [snapshot.md](snapshot.md#一致性) |
| 快照同步频率 | **设了顶替的项目每 15 分钟，其余每小时** | kopia 是增量的，频率高也不会占用太多资源 | [snapshot.md](snapshot.md#频率) |
| 异地副本 | **顶替节点，或只同步不顶替的备份节点** | 复用同步代码；S3 以后再加 | [snapshot.md](snapshot.md#异地副本) |
| 版本记录随项目走 | **同步时带上 project.db 的一致副本** | 顶替、迁移以后还能接着回滚 | [snapshot.md](snapshot.md#备份节点的实现) |
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
