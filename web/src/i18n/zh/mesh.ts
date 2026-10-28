// mesh 状态：节点页和设置页「Mesh」。
export default {
  path: {
    direct: "UDP 直连",
    link: "控制连接",
    udpRelay: "UDP 经面板中转",
    tlsRelay: "TLS 经面板中转",
    none: "不可达",
  },
  address: "地址",
  udpPort: "UDP 端口",
  endpoints: "直连地址",
  peer: "节点 / 面板",
  pathColumn: "路径",
  lastHandshake: "最近握手",
  rxTx: "接收 / 发送",
  noPeers: "暂无连接",
  title: "连接",
  help: "节点之间、节点与面板之间优先使用 UDP 直连；无法直连时经面板中转。",
};
