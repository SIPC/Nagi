// 项目列表、新建项目、选择节点和容器目录。
import type { ReactNode } from "react";

export default {
  attention: {
    title: (n: number) => `需要处理 ${n} 项`,
    // 标题后面的简短名单，几项连在一起显示。
    nodes: (names: string[]) => (names.length === 1 ? `节点 ${names[0]} 离线` : `${names.length} 个节点离线`),
    standby: "备用面板未连接",
    totp: "未开启两步验证",
    masterKey: "未备份主密钥",
    view: "查看",
    collapse: "收起",
  },
  reminders: {
    totpTitle: "未开启两步验证",
    totpText: (link: ReactNode) => <>可在{link}中开启。</>,
    totpLink: "账号安全设置",
    masterKeyTitle: "未备份主密钥",
    masterKeyText: (path: ReactNode) => <>面板数据库中的密钥均由主密钥加密，主密钥丢失后无法解密，须单独备份 {path}。</>,
    dismiss: "不再提醒",
  },
  title: "项目",
  description: "一组协同工作的容器，可以分布在不同节点。",
  newShort: "新建",
  newProject: "新建项目",
  empty: {
    title: "暂无项目",
    text: "新建项目后，可添加容器或容器组（一份 Compose），并为每个容器选择运行的节点。",
  },
  filter: {
    placeholder: "筛选项目或容器…",
    label: "筛选项目或容器",
    sort: "排序",
    byCreated: "最近创建",
    byName: "按名称",
    empty: "没有匹配的项目",
  },
  offline: {
    body: "节点上的容器状态未知，容器可能仍在运行。",
    action: "查看节点",
  },
  standbyOffline: {
    title: (names: string) => `备用面板 ${names} 未连接`,
    body: "备用面板没有同步本面板的数据，主面板故障时无法接管。",
    action: "查看备用面板",
  },
  card: {
    stateful: "有状态",
    noContainers: "暂无容器",
    tookOver: "已顶替",
    failover: "已设顶替",
    imageUpdate: "镜像可更新",
    containers: (n: number) => `${n} 个容器`,
    portMappings: (n: number) => `${n} 个端口映射`,
    moreContainers: (n: number) => `还有 ${n} 个`,
    rebuiltAt: (date: string) => `重建于 ${date}`,
    createdAt: (date: string) => `创建于 ${date}`,
  },
  nodeChoice: {
    node: "节点",
    nodeHelp: "容器在此节点上运行，创建后可迁移至其他节点。",
    noOnlineNodes: "无在线节点",
    noOnlineNodesHint: "无在线节点，无法添加容器。",
    viewNodes: "查看节点",
    dir: "容器目录",
    dirHelp:
      "存放容器的运行文件、数据和快照，须为不存在或为空的绝对路径。留空时使用 <项目根目录>/<网络名>/<容器名>；项目在此节点已有容器时沿用同一文件夹，被占用时追加 -2 等后缀。",
  },
  create: {
    editNetwork: "编辑",
    name: "项目名称",
    namePlaceholder: "我的应用",
    networkName: "网络名",
    slugHint: "小写字母、数字和 -",
    networkHelp: "在面板内唯一。已连接本项目的项目通过「容器.网络名」访问本项目的容器。留空时由项目名生成，汉字转为拼音。",
    groupName: "容器组名称",
    submit: "创建项目",
  },
};
