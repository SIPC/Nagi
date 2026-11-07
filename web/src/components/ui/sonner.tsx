// shadcn/ui Sonner：操作结果的轻提示。主题跟随页面的 dark 类，颜色用项目的变量。
import { useEffect, useState } from "react";
import { Toaster as Sonner } from "sonner";

export function Toaster() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  useEffect(() => {
    const ob = new MutationObserver(() => setDark(document.documentElement.classList.contains("dark")));
    ob.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    return () => ob.disconnect();
  }, []);
  return (
    <Sonner
      theme={dark ? "dark" : "light"}
      position="top-center"
      duration={3000}
      // sonner 自带的样式写死了系统字体和一层很淡的阴影，换成界面字体和浮层统一的阴影（深色下也看得出边缘）。
      toastOptions={{ style: { fontFamily: "var(--font-sans)", boxShadow: "var(--shadow-popover)" } }}
      style={
        {
          "--normal-bg": "var(--popover)",
          "--normal-text": "var(--foreground)",
          "--normal-border": "transparent",
          "--border-radius": "var(--radius-lg)",
        } as React.CSSProperties
      }
    />
  );
}
