import { Languages } from "lucide-react";
import { authClient } from "@/lib/api";
import { locale, locales, setLocale, t } from "@/i18n";
import { Button } from "@/components/ui";

export const nextLocale = locales.find((l) => l.value !== locale)!;

// switchLanguage：在中文和英文之间切换，刷新页面后生效。登录后（account）同时保存到账号，换了设备也沿用；
// 保存失败（例如备用面板只读）不影响在本浏览器里切换。
export async function switchLanguage(account: boolean) {
  if (account) {
    try {
      await authClient.setLanguage({ language: nextLocale.value });
    } catch {
      // 见上。
    }
  }
  setLocale(nextLocale.value);
}

// LanguageButton：登录页右上角的语言按钮。
export function LanguageButton() {
  return (
    <Button readOnlyOk variant="ghost" size="icon" onClick={() => switchLanguage(false)} aria-label={t.common.switchLanguage} title={nextLocale.label}>
      <Languages />
    </Button>
  );
}
