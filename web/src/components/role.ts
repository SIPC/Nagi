// 当前账号的角色。规则以面板为准（internal/server/auth/roles.go），界面只是把用不了的入口藏起来。
import { useQuery } from "@tanstack/react-query";
import { authClient } from "@/lib/api";

export function useRole() {
  const me = useQuery({ queryKey: ["me"], queryFn: () => authClient.me({}), staleTime: 60_000 });
  const role = me.data?.role || "admin";
  return { role, isAdmin: role === "admin", isViewer: role === "viewer" };
}
