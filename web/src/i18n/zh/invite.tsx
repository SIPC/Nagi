// 设置页「接收移交」：接入码。

export default {
  title: "接入码",
  description: "在原面板移交节点时使用；添加新机器使用节点页的「添加节点」。",
  help: "接入码 1 小时内有效，可供多个节点使用。通过接入码加入的节点须在节点页批准后方可使用，以防接入码泄露后被他人利用。",
  generate: "生成接入码",
  pasteHint: (command: string) => `在原面板移交节点时粘贴此接入码，或在节点上执行 ${command}。`,
  command: "nagi panel add <接入码>",
  id: "接入码 ID",
  uses: "使用次数",
  expires: "到期时间",
  usesCount: (n: number) => `${n} 次`,
  revoke: "作废",
  revokeConfirm: (id: string) => `作废接入码 ${id}？作废后不能再用它接入节点。`,
  usedCount: (n: number) => `已用 ${n} 次`,
  expiresIn: (time: string) => `${time}到期`,
  empty: "暂无有效的接入码",
};
