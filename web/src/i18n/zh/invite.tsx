// 设置页「接收节点」：接入码。
import type { ReactNode } from "react";

export default {
  description: (addNode: ReactNode) => <>其他面板移交节点时使用；添加新机器请使用{addNode}。</>,
  addNode: "添加节点",
  help: "接入码 1 小时内有效，可供多个节点使用。通过接入码加入的节点须在节点页批准后方可使用，以防接入码泄露后被他人利用。",
  generate: "生成接入码",
  pasteHint: (command: string) => `在原面板移交节点时粘贴此接入码，或在节点上执行 ${command}。`,
  command: "nagi panel add <接入码>",
  revoke: "撤销",
  revokeAction: "撤销接入码",
  revokeConfirm: (id: string) => `撤销接入码 ${id}？撤销后不能再用它接入节点。`,
  usedCount: (n: number) => `已用 ${n} 次`,
  expiresIn: (time: string) => `${time}到期`,
  empty: "暂无有效的接入码",
};
