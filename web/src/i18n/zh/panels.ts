// 节点移交给其他面板、节点信任的面板列表。
export default {
  handover: {
    confirm: (n: number) =>
      `移交选中的 ${n} 个节点？本面板将不再管理这些节点及仅部署在其上的项目，新面板从节点缓存恢复这些项目。`,
    intro:
      "接入码在新面板「设置」的「接收节点」中生成。新面板接管后，本面板从节点上移除自身，不再管理这些节点。移交以项目为单位，项目相关节点（容器所在节点、顶替节点、备份节点、见证人）须一并选中。",
    code: "接入码",
    codePlaceholder: "nagi-invite:…",
    byProject: "按项目选择",
    byProjectHelp: "选中项目时，同时选中其容器所在节点、备份节点、顶替节点和见证人节点。",
    nodes: "节点",
    offline: "（离线）",
    processing: "处理中…",
    submit: "移交节点",
    handedOver: "已移交",
    failed: "失败",
    finish: "完成",
    codeRequired: "填写接入码",
    nodesRequired: "选择要移交的节点",
  },
  nodePanels: {
    title: "信任的面板",
    intro: "节点接受这些面板的连接和操作。",
    empty: "暂无面板",
    self: "本面板",
    primary: "主面板",
    standby: "备用面板",
    connected: "已连接",
    disconnected: "未连接",
    publicKey: "公钥",
    remove: "移除面板",
    lastPanel: "节点至少要信任一个面板",
    removeSelfConfirm: "从节点上移除本面板？本面板将不再管理该节点，节点由其他面板继续管理。",
    removeConfirm: (address: string, key: string) =>
      `从节点上移除面板 ${address}（公钥 ${key}…）？该面板将无法再连接和操作此节点。`,
  },
};
