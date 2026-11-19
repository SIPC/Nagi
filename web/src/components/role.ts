// 当前账号的角色和权限。规则以面板为准（internal/server/auth/perms.go），界面只是把用不了的入口藏起来。
import { useQuery } from "@tanstack/react-query";
import { authClient } from "@/lib/api";

// 权限名称，和 internal/server/auth/perms.go 的 permNames 一致。
export const permissionGroups = {
  project: [
    "project.view",
    "project.config",
    "project.logs",
    "project.control",
    "project.deploy",
    "project.terminal",
    "project.files.read",
    "project.files.write",
    "project.snapshots",
    "project.backup",
    "project.ha",
    "project.migrate",
    "project.network",
    "project.addresses",
    "project.alerts",
    "project.registries",
    "project.manage",
    "project.privileged",
  ],
  node: ["node.view", "node.details", "node.manage", "node.images", "node.root"],
  panel: ["panel.members", "panel.settings", "panel.audit", "panel.security"],
} as const;

export type Permission = (typeof permissionGroups)[keyof typeof permissionGroups][number];

// 只给管理员的权限：角色里不能勾。
export const adminOnlyPermissions: Permission[] = ["panel.security"];

// requires：勾了一项权限时自动带上的前提，和面板的 Normalize 一致。
const requires: Partial<Record<Permission, Permission[]>> = {
  "project.deploy": ["project.config"],
  "project.privileged": ["project.config", "project.deploy"],
  "project.files.write": ["project.files.read"],
  "project.addresses": ["project.network"],
  "node.manage": ["node.details"],
  "node.root": ["node.details"],
};

// withPrerequisites 补上前提：项目的任何一项都要「查看」，节点的任何一项都要「查看节点」。
export function withPrerequisites(perms: Iterable<string>): Set<string> {
  const out = new Set(perms);
  for (let i = 0; i < 3; i++) {
    for (const p of [...out]) for (const r of requires[p as Permission] ?? []) out.add(r);
    if ([...out].some((p) => p.startsWith("project."))) out.add("project.view");
    if ([...out].some((p) => p.startsWith("node."))) out.add("node.view");
  }
  return out;
}

// dependents：去掉一项权限时一起去掉的（以它为前提的）。
export function withoutDependents(perms: Iterable<string>, removed: string): Set<string> {
  const out = new Set(perms);
  out.delete(removed);
  let changed = true;
  while (changed) {
    changed = false;
    for (const p of [...out]) {
      const needs = [...(requires[p as Permission] ?? []), ...(p.startsWith("project.") && p !== "project.view" ? ["project.view"] : []), ...(p.startsWith("node.") && p !== "node.view" ? ["node.view"] : [])];
      if (needs.some((n) => !out.has(n))) {
        out.delete(p);
        changed = true;
      }
    }
  }
  return out;
}

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: () => authClient.me({}), staleTime: 60_000 });
}

export function useRole() {
  const me = useMe();
  const role = me.data?.role || "admin";
  const isAdmin = role === "admin";
  // 还没拿到时按管理员处理（和以前一样，避免闪一下）；拿到后以面板给的权限为准。
  const perms = new Set(me.data?.permissions ?? []);
  const can = (perm: Permission) => isAdmin || !me.data || perms.has(perm);
  return {
    role,
    isAdmin,
    // isViewer：看不到配置（旧的只读角色）。
    isViewer: !can("project.config"),
    can,
    permissions: perms,
    roleId: me.data?.roleId ?? 0n,
    roleName: me.data?.roleName ?? "",
    roleBuiltin: me.data?.roleBuiltin ?? "",
    scope: me.data?.scope,
  };
}

export type Can = (perm: Permission) => boolean;

// 修改类的权限：一项都没有时页面整体只读（按钮置灰），见 layout.tsx。
export const projectWritePerms: Permission[] = [
  "project.control",
  "project.deploy",
  "project.terminal",
  "project.files.write",
  "project.snapshots",
  "project.backup",
  "project.ha",
  "project.migrate",
  "project.network",
  "project.alerts",
  "project.registries",
  "project.manage",
];
export const nodeWritePerms: Permission[] = ["node.manage", "node.images", "node.root"];

// canProjectSettings：项目设置里有自己能看的部分（基本设置要「项目管理」，其余各节要相应的权限），否则不显示「设置」。
export const canProjectSettings = (can: Can) =>
  (["project.manage", "project.snapshots", "project.alerts", "project.network", "project.config"] as Permission[]).some((p) => can(p));
