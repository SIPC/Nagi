import { pinyin } from "pinyin-pro";
import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";
import { locale, t } from "@/i18n";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

const rtf = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
const rtfNumeric = new Intl.RelativeTimeFormat(locale, { numeric: "always" });

// 已经发生的事的相对时间，例如“3 分钟前”。英文是小写（"just now"），可以直接嵌在句子里；单独显示时用 capitalize。
// 面板的时钟比浏览器快时，刚发生的事会落在“未来”，按“刚刚”显示，不显示成“即将”。
export function timeAgo(date: Date | undefined): string {
  if (!date) return t.common.time.never;
  return relative(Math.min(0, Math.round((date.getTime() - Date.now()) / 1000)));
}

// 将来的时间，例如到期时间“2 小时后”，一分钟内是“即将”。
export function timeUntil(date: Date | undefined): string {
  if (!date) return t.common.time.never;
  const seconds = Math.round((date.getTime() - Date.now()) / 1000);
  if (seconds > 0 && seconds < 60) return t.common.time.soon;
  return relative(seconds);
}

function relative(seconds: number): string {
  const abs = Math.abs(seconds);
  // 只有「昨天」用词语，其余写数字；中文「秒钟前」写成「秒前」。
  const fmt = (v: number, unit: Intl.RelativeTimeFormatUnit) => (unit === "day" && v === -1 ? rtf : rtfNumeric).format(v, unit).replace(/(\d)\s*([^\d\s])/, "$1 $2").replace("秒钟", "秒"); // i18n-ignore
  if (abs < 60) return seconds > -10 ? t.common.time.justNow : fmt(seconds, "second");
  if (abs < 3600) return fmt(Math.round(seconds / 60), "minute");
  if (abs < 86400) return fmt(Math.round(seconds / 3600), "hour");
  return fmt(Math.round(seconds / 86400), "day");
}

// capitalize 把首字母大写，用于单独显示的英文短语（中文不受影响）。
export function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// 日期和时间按界面语言格式化（不要直接调 toLocaleString()，它按浏览器语言）。
export function formatDateTime(date: Date | undefined): string | undefined {
  return date?.toLocaleString(locale);
}

export function formatDate(date: Date | undefined): string | undefined {
  return date?.toLocaleDateString(locale, { year: "numeric", month: "2-digit", day: "2-digit" });
}

export function formatTime(date: Date | undefined): string | undefined {
  return date?.toLocaleTimeString(locale);
}

// formatClock：只有时和分，24 小时制，例如 08:12。用在今天、近期这类短时间里。
export function formatClock(date: Date): string {
  return date.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit", hour12: false });
}

// formatMonthDay：月和日，不是今年时带上年份，例如 10/28、2025/10/28。
export function formatMonthDay(date: Date): string {
  const sameYear = date.getFullYear() === new Date().getFullYear();
  return date.toLocaleDateString(locale, sameYear ? { month: "2-digit", day: "2-digit" } : { year: "numeric", month: "2-digit", day: "2-digit" });
}

// slugName 由名字生成小写字母、数字和 - 组成的标识（不超过 32 个字符）；
// 名字里没有能用的字符（例如全是中文）时用 fallback（通常是 ID 的前几位）。
// slugName 把名称转成小写字母、数字和 - 组成的短名（不超过 32 个字符），汉字转成不带声调的拼音
// （"我的博客" → "wo-de-bo-ke"）。和面板的 spec.Slug 规则一致。没有能用的字符时返回 fallback。
export function slugName(name: string, fallback: string) {
  const s = name
    .toLowerCase()
    .replace(/[\u4e00-\u9fff]+/g, (han) => `-${pinyin(han, { toneType: "none", type: "array" }).join("-")}-`)
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 32)
    .replace(/-+$/, "");
  return s || fallback;
}
