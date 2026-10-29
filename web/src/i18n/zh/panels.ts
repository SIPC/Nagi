// 节点移交给其他面板、节点信任的面板列表。
export default {
  handover: {
    confirm: (n: number) =>
      `移交选中的 ${n} 个节点？本面板将不再管理这些节点及仅部署在其上的项目，新面板从节点缓存恢复这些项目。`,
    failedName: "移交",
    intro:
      "接入码在新面板的「设置」页生成。新面板接管后，本面板从节点上移除自身，不再管理这些节点。移交以项目为单位，项目相关节点（容器所在节点、顶替节点、备份节点、见证人）须一并选中。",
    code: "接入码",
    codeHint: "格式：nagi-invite:…",
    codePlaceholder: "粘贴其他面板生成的接入码",
    byProject: "按项目选择",
    byProjectHelp: "选中项目时，同时选中其容器所在节点、备份节点、顶替节点和见证人节点。",
    nodes: "节点",
    offline: "（离线）",
    processing: "处理中…",
    submit: "移交节点",
  },
  nodePanels: {
    title: "信任的面板",
    intro: "节点接受这些面板的连接和操作。",
    columns: {
      panel: "面板",
      status: "状态",
    },
    empty: "暂无面板",
    self: "本面板",
    primary: "主面板",
    standby: "备用面板",
    connected: "已连接",
    disconnected: "未连接",
    removeSelfConfirm: "从节点上移除本面板？本面板将不再管理该节点，节点由其他面板继续管理。",
    removeConfirm: (address: string, key: string) =>
      `从节点上移除面板 ${address}（公钥 ${key}…）？该面板将无法再连接和操作此节点。`,
  },
};
