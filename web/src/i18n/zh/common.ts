// 通用的词：状态、按钮、时间。状态用固定的词（docs/ui.md）。
export default {
  // 字段和标题旁问号按钮的读屏名称。
  help: "说明",
  status: {
    running: "运行中",
    stopped: "已停止",
    restarting: "重启中",
    paused: "已暂停",
    created: "已创建",
    abnormal: "异常",
    removing: "删除中",
    notDeployed: "未部署",
    unhealthy: "不健康",
    starting: "启动中",
    partial: (running: number, total: number) => `部分运行 ${running} / ${total}`,
    online: "在线",
    offline: "离线",
    unknown: "状态未知",
    nodesOffline: (names: string[]): string => (names.length === 1 ? `节点 ${names[0]} 离线` : `${names.length} 个节点离线`),
    syncing: "同步中",
    pendingApproval: "待批准",
  },
  // 把两句说明接起来时用。
  sentenceEnd: "。",
  // 把几个名字连成一串，例如「web、db」。
  list: (items: string[]) => items.join("、"),
  actions: {
    save: "保存",
    cancel: "取消",
    delete: "删除",
    remove: "移除",
    confirm: "确认",
    ok: "好",
    close: "关闭",
    copy: "复制",
    edit: "编辑",
    add: "添加",
    create: "创建",
    refresh: "刷新",
    retry: "重试",
    more: "更多操作",
    back: "返回",
  },
  feedback: {
    loading: "加载中",
    saved: "已保存",
    copied: "已复制",
    copyFailed: "无法复制，手动选中后复制",
    done: "已完成",
    // 接口没有返回原因时的兜底。
    failed: "面板未返回原因，可在「活动」中查看",
    viewActivity: "查看活动",
    // 操作输出里需要注意的几行合并成一条提示。
    notes: (n: number) => `需要注意 ${n} 项`,
    showNotes: "查看",
    hideNotes: "收起",
  },
  errors: {
    http: (status: string) => `面板返回 HTTP ${status}。面板可能正在重启，或页面版本过旧，可刷新页面后重试`,
    unreachable: "无法连接面板，检查网络或面板是否在运行",
  },
  unsaved: {
    confirm: "离开此页？未保存的修改将丢失。",
    leave: "离开",
  },
  time: {
    never: "从未",
    justNow: "刚刚",
    // 将来的时间不到 1 分钟时，例如接入码即将到期。
    soon: "即将",
    seconds: (n: number) => `${n} 秒`,
    minutes: (n: number) => `${n} 分钟`,
    hours: (n: number) => `${n} 小时`,
    days: (n: number) => `${n} 天`,
  },
  readOnly: {
    standby: "备用面板只能查看，修改请到主面板",
    role: "当前角色只能查看",
  },
  language: "语言",
  switchLanguage: "切换语言",
};
