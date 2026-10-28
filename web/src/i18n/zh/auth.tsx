// 登录、初始化页面。
import type { ReactNode } from "react";

export default {
  username: "用户名",
  password: "密码",
  setup: {
    title: "初始化面板",
    description: "设置管理员账号，用于登录面板。",
    missingToken: "缺少初始化令牌",
    missingTokenHint: "请使用面板启动日志中输出的初始化链接打开本页。",
    passwordMismatch: "两次输入的密码不一致",
    passwordHint: "至少 8 个字符",
    confirmPassword: "确认密码",
    submit: "完成初始化",
  },
  login: {
    title: "登录",
    totpTitle: "两步验证",
    description: "使用管理员账号登录面板。",
    totpDescription: "输入验证器应用中的 6 位验证码。",
    notInitialized: "面板尚未初始化",
    notInitializedHint: (url: ReactNode) => <>暂无可登录的账号。请打开面板启动日志中输出的初始化链接（{url}）设置管理员账号。</>,
    setupUrl: "https://地址:端口/setup?token=…",
    code: "验证码",
    submit: "登录",
    verifySubmit: "验证并登录",
  },
};
