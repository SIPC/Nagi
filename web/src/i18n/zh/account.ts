// 导航底部的用户卡片和个人资料。
export default {
  signedInAs: (name: string) => `登录账号：${name}`,
  profile: "个人资料",
  security: "账号安全",
  upload: "上传头像",
  remove: "移除",
  avatarHint: "支持 PNG、JPEG、WebP、GIF，不超过 20 MB，会裁成正方形。",
  notImage: "无法读取此图片，须为 PNG、JPEG、WebP 或 GIF 格式",
  tooLarge: "图片不能超过 20 MB",
  displayName: "显示名称",
  displayNameHint: (username: string) => `留空时显示 ${username}。登录时仍然使用用户名。`,
  saved: "个人资料已保存",
  // 重新验证身份（见 docs/panel.md#会话）。
  reverify: {
    title: "验证身份",
    description: "登录状态需要确认，验证后继续使用。",
    reasons: {
      session_key: "无法确认这台设备，验证后继续使用。",
      signature_invalid: "无法确认这台设备，验证后继续使用。",
      tls_changed: "浏览器的连接特征发生了变化，验证后继续使用。",
      network_flip: "网络在短时间内来回切换，验证后继续使用。",
      network_changed: "登录所在的网络发生了变化，验证后继续使用。",
    },
    password: "密码",
    code: "两步验证码",
    submit: "验证",
    logout: "退出登录",
  },
};
