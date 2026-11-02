// 设置页：标题和左侧二级菜单。
export default {
  title: "设置",
  groups: { account: "我的账号", panel: "面板", nodes: "节点", records: "记录和告警" },
  tabs: {
    profile: { label: "个人资料", description: "显示名称和头像。" },
    access: { label: "面板访问", description: "节点和浏览器访问本面板所用的地址。" },
    security: { label: "账号安全", description: "登录密码、两步验证和安全入口。" },
    members: { label: "成员", description: "可以登录本面板的账号和它们的角色。" },
    alerts: { label: "告警", description: "节点和容器异常时的通知渠道和触发规则。" },
    activity: { label: "活动记录", description: "面板上的操作和发出的告警，保留最近 5000 条。" },
    standby: { label: "备用面板", description: "实时同步本面板数据的备用面板，主面板故障时提升为主面板。" },
    invite: { label: "接收节点", description: "其他面板向本面板移交节点时使用的接入码。" },
    mesh: { label: "节点网络", description: "面板在节点专用网络中的地址及与各节点的连接。" },
    update: { label: "版本更新", description: "查看面板版本，一键更新到最新发布的版本。" },
  },
};
