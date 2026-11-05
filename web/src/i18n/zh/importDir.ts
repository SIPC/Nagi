// 添加容器里的「现有目录」：把节点上已有的 compose 目录接入面板。
export default {
  description: "接入节点上已有的 compose 目录。数据留在原处，目录里的 .env 复制为容器组变量，部署时停止原来运行的容器，由面板接管。",
  dir: "目录",
  dirHelp: "节点上 compose 文件所在目录的绝对路径。",
  read: "读取",
  elevate: "读取节点上的目录需验证身份，验证后 15 分钟内有效。",
  composeFile: "compose 文件",
  variables: "导入的变量",
  none: "无",
  replaces: "将替换",
  nothingRunning: "目录中没有运行中的容器",
  replaceNotice: "部署时会停止这些容器，新容器启动后删除它们（数据和卷保留）；启动失败则恢复原来的容器。",
  limits: "数据不在容器目录中：快照不包含这些数据，也不能迁移、顶替或设置备份节点。",
  importAndDeploy: "接入并部署",
};
