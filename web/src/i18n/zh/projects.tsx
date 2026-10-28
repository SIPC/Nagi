// 项目列表、新建项目、选择节点和容器目录。
import type { ReactNode } from "react";

export default {
  reminders: {
    totpTitle: "未开启两步验证",
    totpText: (link: ReactNode) => <>建议在{link}中开启。</>,
    totpLink: "设置 › 账号安全",
    masterKeyTitle: "备份面板主密钥",
    masterKeyText: (path: ReactNode) => <>面板数据库中的密钥均由主密钥加密，主密钥丢失后无法解密。请单独备份 {path}。</>,
    dismiss: "不再提醒",
  },
  title: "项目",
  description: "项目由一组协同工作的容器组成，容器可分布在不同节点，并按名称互相访问。",
  newProject: "新建项目",
  empty: {
    title: "暂无项目",
    text: "新建项目后，可添加容器或容器组（一份 compose），并为每个容器选择运行的节点。",
  },
  card: {
    stateful: "有状态",
    noContainers: "暂无容器",
    nodeOffline: "节点离线",
    tookOver: "已顶替",
    failover: "顶替",
    containers: (n: number) => `${n} 个容器`,
    portMappings: (n: number) => `${n} 个端口映射`,
    rebuiltAt: (date: string) => `重建于 ${date}`,
    createdAt: (date: string) => `创建于 ${date}`,
  },
  nodeChoice: {
    node: "节点",
    nodeHelp: "容器在此节点上运行，创建后可迁移至其他节点。",
    noOnlineNodes: "无在线节点",
    dir: "容器目录",
    dirHelp:
      "存放容器的运行文件、数据和快照。须为绝对路径，且目录不存在或为空。留空时使用默认目录：<项目根目录>/<项目名>/<容器名>，项目名转为拼音，重名时追加 -2 等后缀。",
  },
  create: {
    description: "创建后不自动部署，确认配置后可手动部署。",
    basics: "基本信息",
    name: "名称",
    namePlaceholder: "我的应用",
    networkName: "网络名",
    slugHint: "小写字母、数字和 -",
    networkHelp: "在面板内唯一。已连接本项目的项目通过「容器.网络名」访问本项目的容器。留空时由项目名生成，汉字转为拼音。",
    firstContainer: "第一个容器",
    firstContainerDescription: "可选，创建项目后也可继续添加容器。",
    withContainer: "同时创建",
    groupName: "容器组名称",
    containerName: "容器名称",
    containerNameHelp: "创建后不可修改。单个容器在项目内网中使用此名称；容器组内的容器名称为「容器.容器组名」。",
    submit: "创建项目",
  },
};
