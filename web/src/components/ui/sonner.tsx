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
