// mesh 状态：节点页和设置页「Mesh」。
export default {
  path: {
    direct: "UDP 直连",
    link: "控制连接",
    udpRelay: "UDP 经面板中转",
    tlsRelay: "TLS 经面板中转",
    none: "不可达",
  },
  pathShort: {
    direct: "直连",
    relay: "经面板中转",
  },
  address: "地址",
  udpPort: "UDP 端口",
  endpoints: "直连地址",
  peer: "节点 / 面板",
  pathColumn: "路径",
  lastHandshake: "最近握手",
  lastHandshakeAgo: (ago: string) => `最近握手 ${ago}`,
  noHandshake: "尚未握手",
  pingTimeout: "超时",
  pingFailed: "失败",
  rxTx: "接收 / 发送",
  noPeers: "暂无连接",
  fec: (m: number) => `纠错 ${m}/10`,
  fecDetail: (loss: string, recovered: string) => `对方丢包 ${loss}，已恢复 ${recovered} 个包`,
  title: "连接",
};
