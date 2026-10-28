// 界面语言。启动时确定一次，之后不变；切换语言时刷新页面，所以 t 可以在任何地方直接用，包括模块顶层的常量。
//
// 选择顺序：浏览器里记住的选择（localStorage）→ 账号保存的语言（登录后，浏览器里没有选过时采用，见 adoptAccountLocale）
// → 浏览器的语言偏好（navigator.languages，也就是 Accept-Language）。接口请求带上 Accept-Language: locale，
// 面板按它返回错误信息（见 lib/api.ts）。
import type zh from "./zh";

export type Messages = typeof zh;

export type Locale = "zh-CN" | "en";

// 语言名用各自的语言写，不翻译。
export const locales: { value: Locale; label: string }[] = [
  { value: "zh-CN", label: "中文" },
  { value: "en", label: "English" },
];

const storageKey = "lang";

export function matchLocale(tag: string | undefined): Locale | undefined {
  const base = tag?.toLowerCase().split(/[-_]/)[0];
  return base === "zh" ? "zh-CN" : base === "en" ? "en" : undefined;
}

function storedLocale(): Locale | undefined {
  try {
    return matchLocale(localStorage.getItem(storageKey) ?? undefined);
  } catch {
    return undefined;
  }
}

function browserLocale(): Locale {
  for (const tag of navigator.languages?.length ? navigator.languages : [navigator.language]) {
    const l = matchLocale(tag);
    if (l) return l;
  }
  return "en";
}

function remember(l: Locale) {
  try {
    localStorage.setItem(storageKey, l);
  } catch {
    // 隐私模式等情况下无法保存，本次刷新后仍按浏览器语言显示。
  }
}

export const locale: Locale = storedLocale() ?? browserLocale();

// 只加载当前语言的文案。顶层 await：引用 t 的模块会等它加载完再执行。
export const t: Messages = (await (locale === "en" ? import("./en") : import("./zh"))).default;

document.documentElement.lang = locale;

// setLocale 记住选择并刷新页面。保存到账号由调用方负责（见 components/language.tsx）。
export function setLocale(l: Locale) {
  remember(l);
  location.reload();
}

// adoptAccountLocale 在登录后调用：浏览器里没有选过语言、账号保存过时，采用账号的（例如换了设备）。
export function adoptAccountLocale(saved: string) {
  const l = matchLocale(saved);
  if (!l || storedLocale()) return;
  remember(l);
  if (l !== locale) location.reload();
}
