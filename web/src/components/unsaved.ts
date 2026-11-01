// 表单有未保存的修改时，离开页面前先确认。刷新、关闭标签页时用浏览器自带的提示。
import { useBlocker } from "@tanstack/react-router";
import { useRef } from "react";
import { confirmAction } from "@/components/confirm";
import { t } from "@/i18n";

const stringify = (v: unknown) => JSON.stringify(v, (_, x) => (typeof x === "bigint" ? x.toString() : x));

// useUnsaved 记住第一次渲染时的内容，返回现在有没有改动，有改动时拦截离开。when 为 false 时不拦截。
export function useUnsaved(value: unknown, when = true): boolean {
  const now = stringify(value);
  const initial = useRef(now);
  const dirty = when && now !== initial.current;
  useBlocker({
    shouldBlockFn: async () => !(await confirmAction(t.common.unsaved.confirm, { action: t.common.unsaved.leave, danger: true })),
    disabled: !dirty,
    enableBeforeUnload: dirty,
  });
  return dirty;
}
