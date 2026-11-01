// 设置页「版本更新」。
export default {
  badge: "有新版本",
  current: "当前版本",
  latest: "最新版本",
  check: "检查更新",
  upToDate: "已是最新版本。",
  availableTitle: (version: string) => `可更新到 ${version}`,
  text: "面板从 GitHub Release（安装时指定了镜像则从镜像）下载新版本，校验后原地重启，页面会中断几秒；项目容器照常运行。",
  notes: "查看更新说明",
  primaryHint: "备用面板要分别更新：两边的数据库版本不同时，备用面板会暂停同步，直到更新到同一版本。",
  standbyHint: "这里只更新本备用面板。和主面板的数据库版本不同时会暂停同步，直到两边更新到同一版本。",
  action: "更新",
  running: "下载中…",
  confirm: (version: string) => `将面板更新到 ${version}？面板下载并校验新版本后原地重启，进行中的部署等操作会中断。更新后不能在面板上退回旧版本。`,
  restartingTitle: (version: string) => `已替换为 ${version}，面板正在重启`,
  restartingText: "重启好后页面会自动刷新。之后可以到节点页升级各节点。",
  timeout: (version: string) => `面板已替换为 ${version}，但 3 分钟内没有恢复。请到服务器上查看日志：journalctl -u nagi-server -n 50`,
};
