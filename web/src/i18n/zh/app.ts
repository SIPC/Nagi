// 外框（导航、备用面板提示）、终端、操作输出和端口格式检查。
export default {
  nav: {
    projects: "项目",
    nodes: "节点",
    settings: "设置",
  },
  theme: {
    toLight: "切换为浅色",
    toDark: "切换为深色",
    light: "浅色",
    dark: "深色",
  },
  logout: "退出登录",
  standby: {
    title: "备用面板",
    readOnly: "只能查看，修改请到主面板",
    synced: "已同步",
    syncedAgo: (ago: string) => `已同步（${ago}）`,
    disconnected: "未连接主面板",
    disconnectedAgo: (ago: string) => `未连接主面板，最近同步 ${ago}`,
  },
  terminal: {
    connecting: "正在连接…",
    exited: (code: number) => `进程已退出（退出码 ${code}）`,
    closed: "连接已关闭",
    intro: "在容器内打开 shell。",
    noRunning: "无运行中的容器。",
    connect: "连接",
    reconnect: "重新连接",
  },
  ports: {
    badFormat: (p: string) => `「${p}」格式不对，应为 节点端口:容器端口，例如 8080:80 或 53:53/udp`,
    outOfRange: (p: string) => `「${p}」端口应在 1～65535 之间`,
    reserved: (published: number, target: number) =>
      `节点端口 ${published} 留给域名访问。网站请换一个端口（如 8080:${target}），或部署后在「网络」里绑定域名`,
  },
};
