// 导航底部的用户卡片和个人资料。
export default {
  menu: "账号菜单",
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
};
