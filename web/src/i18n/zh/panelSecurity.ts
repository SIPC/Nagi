// 设置 → 安全（见 docs/panel.md#会话）。
export default {
  recording: {
    title: "终端录制",
    description: "录下节点终端、容器终端和命令执行的输出，管理员可以在审计页回放。",
    help: "只录输出，不录输入：不回显的密码不会进入录制。录制用面板主密钥加密，保留 90 天。关闭需要验证身份，并会发送告警。",
    label: "录制终端和命令输出",
    elevate: "关闭终端录制前需要验证身份。",
  },
  session: {
    title: "会话",
    description: "登录状态的有效期，以及网络变化时是否要求重新验证。",
    help: "会话令牌每 15 分钟更换一次，只有登录时的浏览器能完成更换。被复制到别处使用的登录会被退出并告警。",
    idle: "空闲超时",
    idleDescription: "超过这个时间没有操作，退出登录。",
    maxAge: "登录有效期",
    maxAgeDescription: "从登录起最长保持的时间。",
    bind: "管理员网络绑定",
    bindDescription: "管理员的网络变化后要求重新验证身份。",
    bindHelp: "网段按 IPv4 /24、IPv6 /48 计算。IPv4 和 IPv6 之间的切换不算变化。",
    bindSubnet: "网段变化时",
    bindIP: "IP 变化时",
    bindOff: "关闭",
    bindAll: "用于所有成员",
    bindAllDescription: "操作员和只读成员也按上面的方式绑定网络。",
    minutes: (n: number) => (n % 60 === 0 ? `${n / 60} 小时` : `${n} 分钟`),
    hours: (n: number) => (n % 24 === 0 ? `${n / 24} 天` : `${n} 小时`),
  },
};
