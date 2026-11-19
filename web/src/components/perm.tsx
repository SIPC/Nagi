// 按权限置灰：没有修改权限时里面的按钮都置灰（和备用面板的只读一样，查看用的按钮加 readOnlyOk）。
// 没有查看权限的入口直接不显示，不用这个。规则以面板为准（internal/server/auth/perms.go）。
import { useContext } from "react";
import { ReadOnlyContext } from "@/components/ui";

export function WriteGate({ allowed, children }: { allowed: boolean; children: React.ReactNode }) {
  const readOnly = useContext(ReadOnlyContext);
  return <ReadOnlyContext.Provider value={readOnly || !allowed}>{children}</ReadOnlyContext.Provider>;
}
