// 设置页「关于」（pages/about.tsx）。
export default {
  tagline: "跨节点部署和管理容器的自托管面板。",
  update: {
    title: "更新",
    help: "正式版渠道只更新到正式版。预发布渠道还会更新到预发布版，用于在正式发布前试用。仅影响本面板；备用面板和各节点分别设置。",
  },
  links: {
    title: "链接",
    source: "源代码",
    changelog: "更新记录",
    issues: "反馈问题",
    license: "开源许可",
  },
  credits: {
    title: "致谢",
    caddy: "反向代理和自动 HTTPS，含 caddy-l4",
    wireguard: "节点之间的加密网络",
    quic: "QUIC 快速通道与 BBR 拥塞控制",
    kopia: "快照与异地副本",
    control: "控制面通信",
    shadcn: "界面组件",
    misans: "界面字体，由小米提供",
  },
};
