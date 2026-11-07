// 在页面渲染前确定深浅色，避免闪烁。单独成文件而不是内联：面板的 CSP 不允许内联脚本。
try {
  const t = localStorage.getItem("theme");
  const dark = t ? t === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  document.documentElement.classList.toggle("dark", dark);
  // 和 index.css 的 --background 一致；切换主题时由 layout.tsx 的 useTheme 更新。
  document.querySelector('meta[name="theme-color"]')?.setAttribute("content", dark ? "#0a0a0a" : "#ffffff");
} catch {}
