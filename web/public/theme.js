// 在页面渲染前确定深浅色，避免闪烁。单独成文件而不是内联：面板的 CSP 不允许内联脚本。
try {
  const t = localStorage.getItem("theme");
  const dark = t ? t === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  document.documentElement.classList.toggle("dark", dark);
} catch {}
