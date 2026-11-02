// 设置页「关于」（pages/about.tsx）。
export default {
  tagline: "跨节点部署和管理容器的自托管面板。",
  upToDate: "已是最新版本",
  available: (version: string) => `可更新到 ${version}`,
  links: {
    title: "项目",
    source: "源代码",
    changelog: "更新记录",
    issues: "反馈问题",
    license: "开源许可",
  },
  credits: {
    title: "致谢",
    caddy: "反向代理和自动 HTTPS，含 caddy-l4",
    wireguard: "节点之间的加密网络",
    kopia: "快照与异地副本",
    control: "控制面通信",
    shadcn: "界面组件",
    misans: "界面字体，由小米提供",
  },
};
