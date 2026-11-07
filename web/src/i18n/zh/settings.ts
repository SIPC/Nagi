// 设置页：标题和左侧二级菜单。
export default {
  title: "设置",
  groups: { account: "我的账号", panel: "面板", nodes: "节点" },
  tabs: {
    profile: { label: "个人资料" },
    access: { label: "面板访问", description: "节点和浏览器访问本面板所用的地址。" },
    security: { label: "账号安全" },
    members: { label: "成员" },
    alerts: { label: "告警" },
    standby: { label: "备用面板" },
    invite: { label: "接收节点" },
    mesh: { label: "节点网络" },
    about: { label: "关于" },
    update: { label: "版本更新" },
  },
};
