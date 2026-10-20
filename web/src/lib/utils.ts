import { pinyin } from "pinyin-pro";
import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

const rtf = new Intl.RelativeTimeFormat("zh-CN", { numeric: "auto" });

// 相对时间，例如“3 分钟前”。
export function timeAgo(date: Date | undefined): string {
  if (!date) return "从未";
  const seconds = Math.round((date.getTime() - Date.now()) / 1000);
  const abs = Math.abs(seconds);
  const fmt = (v: number, unit: Intl.RelativeTimeFormatUnit) => rtf.format(v, unit).replace(/(\d)\s*([^\d\s])/, "$1 $2");
  if (abs < 60) return seconds > -10 ? "刚刚" : fmt(seconds, "second");
  if (abs < 3600) return fmt(Math.round(seconds / 60), "minute");
  if (abs < 86400) return fmt(Math.round(seconds / 3600), "hour");
  return fmt(Math.round(seconds / 86400), "day");
}

// slugName 由名字生成小写字母、数字和 - 组成的标识（不超过 32 个字符）；
// 名字里没有能用的字符（例如全是中文）时用 fallback（通常是 ID 的前几位）。
// slugName 把名称转成小写字母、数字和 - 组成的短名（不超过 32 个字符），汉字转成不带声调的拼音
// （"我的博客" → "wo-de-bo-ke"）。和面板的 spec.Slug 规则一致。没有能用的字符时返回 fallback。
export function slugName(name: string, fallback: string) {
  const s = name
    .toLowerCase()
    .replace(/[一-鿿]+/g, (han) => `-${pinyin(han, { toneType: "none", type: "array" }).join("-")}-`)
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 32)
    .replace(/-+$/, "");
  return s || fallback;
}
