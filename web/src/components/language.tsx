import { Languages } from "lucide-react";
import { authClient } from "@/lib/api";
import { locale, locales, setLocale, t } from "@/i18n";
import { Button } from "@/components/ui";

// LanguageButton：在中文和英文之间切换，刷新页面后生效。登录后（account）同时保存到账号，换了设备也沿用；
// 保存失败（例如备用面板只读）不影响在本浏览器里切换。
export function LanguageButton({ account = false }: { account?: boolean }) {
  const next = locales.find((l) => l.value !== locale)!;
  const onClick = async () => {
    if (account) {
      try {
        await authClient.setLanguage({ language: next.value });
      } catch {
        // 见上。
      }
    }
    setLocale(next.value);
  };
  return (
    <Button readOnlyOk variant="ghost" size="icon" onClick={onClick} aria-label={t.common.switchLanguage} title={next.label}>
      <Languages />
    </Button>
  );
}
