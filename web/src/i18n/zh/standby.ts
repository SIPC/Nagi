// 设置页「备用面板」：提升为主面板、同步状态、已配对的备用面板。
export default {
  promote: {
    confirm:
      "将本面板提升为主面板？\n\n" +
      "· 主面板仍在运行时不应提升，否则两个面板可能产生不一致的修改。\n" +
      "· 提升后，节点仅接受本面板的修改，其他备用面板改为从本面板同步。\n" +
      "· 原主面板恢复后自动降为备用面板，其最后数秒内未同步的修改将丢失。",
    done: (term: bigint) => `已提升为主面板（任期 ${term}）。面板正在重启，页面将自动刷新。`,
    button: "提升为主面板",
  },
  connected: "已连接",
  disconnected: "未连接",
  lastSync: "最近同步",
  status: {
    title: "同步状态",
    description: "本面板是备用面板，实时同步主面板的数据。",
    primary: "主面板",
    connection: "连接",
    neverSynced: "尚未同步",
  },
  list: {
    title: "已配对的备用面板",
    help: "备用面板经面板端口连接本面板，与本面板共用 CA 和主密钥。备用面板为只读，所有修改在主面板上进行。",
    add: "添加备用面板",
    empty: "暂无备用面板",
    name: "名称",
    address: "地址",
    status: "状态",
  },
  removeConfirm: (name: string) => `移除备用面板 ${name}？该备用面板将无法再同步本面板数据，其数据目录须手动删除。`,
  dialog: {
    title: "添加备用面板",
    description: "在新机器上使用配对码启动备用面板。",
    command: "启动命令",
    commandHint: "数据目录须为空；配对码 1 小时内有效，仅可使用一次",
    notice: "备用面板启动后自动连接本面板并开始同步，同时显示在列表中。",
  },
};
