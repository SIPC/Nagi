// 设置页：标题和左侧二级菜单。
export default {
  title: "设置",
  tabs: {
    access: { label: "面板访问", description: "节点和浏览器访问本面板所用的地址。" },
    security: { label: "账号安全", description: "登录密码、两步验证和安全入口。" },
    alerts: { label: "告警", description: "节点和容器异常时的通知渠道和触发规则。" },
    standby: { label: "备用面板", description: "实时同步本面板数据的备用面板，主面板故障时提升为主面板。" },
    invite: { label: "接收移交", description: "其他面板向本面板移交节点时使用的接入码。" },
    mesh: { label: "Mesh", description: "面板在 mesh 网络中的地址及与各节点的连接。" },
  },
};
