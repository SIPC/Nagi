import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useMatch, useNavigate, useRouterState } from "@tanstack/react-router";
import { Boxes, ChevronRight, Server, Settings } from "lucide-react";
import { authClient, nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { Button, Empty, ReadOnlyContext, Waves } from "@/components/ui";
import { PromoteButton, useStandbys } from "@/pages/standby";
import { UpdateDot } from "@/pages/update";
import { UserMenu, type ThemeMode } from "@/components/account";
import { t } from "@/i18n";
import { CommandPalette, SearchButton } from "@/components/command-palette";
import { settingsTabs } from "@/pages/settings";
import type { Node } from "@/gen/nagi/v1/api_pb";
import { useRole } from "@/components/role";

function readTheme(): ThemeMode {
  try {
    const v = localStorage.getItem("theme");
    return v === "light" || v === "dark" ? v : "system";
  } catch {
    return "system";
  }
}

// useTheme：浅色、深色或跟随系统。没有保存过选择时跟随系统（public/theme.js 同样处理）。
function useTheme() {
  const [mode, setMode] = useState<ThemeMode>(readTheme);
  useEffect(() => {
    const media = matchMedia("(prefers-color-scheme: dark)");
    const apply = () => document.documentElement.classList.toggle("dark", mode === "dark" || (mode === "system" && media.matches));
    apply();
    if (mode !== "system") return;
    media.addEventListener("change", apply);
    return () => media.removeEventListener("change", apply);
  }, [mode]);
  const set = (next: ThemeMode) => {
    try {
      if (next === "system") localStorage.removeItem("theme");
      else localStorage.setItem("theme", next);
    } catch {
      // 隐私模式等情况下无法保存，不影响切换。
    }
    setMode(next);
  };
  return { mode, set };
}

// usePageTitle：浏览器标签页显示当前页面，方便在多个标签页之间区分。
function usePageTitle(nodes: Node[] | undefined, isAdmin: boolean) {
  const location = useRouterState({ select: (s) => s.location });
  const parts = location.pathname.split("/").filter(Boolean);
  const projectId = parts[0] === "projects" && parts[1] && parts[1] !== "new" ? parts[1] : "";
  const project = useQuery({ queryKey: ["project", projectId], queryFn: () => projectClient.getProject({ projectId }), enabled: !!projectId, retry: false });
  let title: string = t.app.nav.projects;
  if (parts[0] === "projects" && parts[1] === "new") title = t.app.search.newProject;
  else if (projectId) {
    const name = project.data?.project?.name ?? t.app.nav.projects;
    title = parts[2] === "c" && parts[3] ? `${decodeURIComponent(parts[3])} · ${name}` : name;
  } else if (parts[0] === "nodes") {
    title = parts[1] ? (nodes?.find((n) => n.id === parts[1])?.name ?? t.app.nav.nodes) : t.app.nav.nodes;
  } else if (parts[0] === "settings") {
    const allowed = isAdmin ? settingsTabs : settingsTabs.filter((x) => x.value === "security");
    const tab = allowed.find((x) => x.value === (location.search as { tab?: string }).tab) ?? allowed[0];
    title = `${tab.label} · ${t.app.nav.settings}`;
  }
  useEffect(() => {
    document.title = `${title} · Nagi`;
    return () => {
      document.title = "Nagi";
    };
  }, [title]);
}

// 左侧导航（参照 OpenAI 平台后台）：上面是产品名和三个入口，下面是用户卡片。窄屏时收成顶部一行。
const navItems = [
  { to: "/projects", label: t.app.nav.projects, icon: Boxes },
  { to: "/nodes", label: t.app.nav.nodes, icon: Server },
  { to: "/settings", label: t.app.nav.settings, icon: Settings },
] as const;

const navLink =
  "flex h-8 items-center gap-2.5 rounded-md px-2.5 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground data-[status=active]:bg-accent data-[status=active]:font-medium data-[status=active]:text-foreground [&_svg]:size-4";

export function AppLayout() {
  const theme = useTheme();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const me = useQuery({ queryKey: ["me"], queryFn: () => authClient.me({}) });
  const standby = useStandbys().data;
  // 所有页面共用这份节点列表，定时刷新后状态才能及时更新。
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), refetchInterval: 10000 });
  const offlineCount = (nodes.data?.nodes ?? []).filter((n) => !n.online && !n.pending).length;
  const { isAdmin, isViewer } = useRole();
  usePageTitle(nodes.data?.nodes, isAdmin);
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  // 备用面板全部只读。账号角色：只读角色的项目和节点只能看，操作员的节点只能看；设置页里只有自己的账号，不受影响。
  const readOnly = (standby?.isStandby ?? false) || (!pathname.startsWith("/settings") && (isViewer || (pathname.startsWith("/nodes") && !isAdmin)));
  // 项目页（含容器页）宽屏取消内容区的宽度限制，由 ProjectSplit 分成项目栏和右边的内容。
  const split = !!useMatch({ from: "/app/projects/$projectId", shouldThrow: false });

  const logout = async () => {
    await authClient.logout({});
    queryClient.clear();
    navigate({ to: "/login" });
  };

  const brand = (
    <Link to="/projects" className="flex items-center gap-2 px-2.5 text-[15px] font-semibold tracking-tight">
      <Waves className="h-3.5 w-7 text-wave" />
      Nagi
    </Link>
  );
  const nav = navItems.map((it) => (
    <Link key={it.to} to={it.to} className={navLink}>
      <it.icon />
      {it.label}
      {it.to === "/settings" && <UpdateDot />}
      {it.to === "/nodes" && offlineCount > 0 && (
        <span className="ml-auto rounded-full bg-destructive px-1.5 text-[11px] leading-4 font-medium text-white tabular-nums" title={t.app.nodesOffline(offlineCount)}>
          {offlineCount}
        </span>
      )}
    </Link>
  ));
  const userMenu = (compact: boolean) => (
    <UserMenu me={me.data} theme={theme.mode} onTheme={theme.set} onLogout={logout} readOnly={standby?.isStandby ?? false} compact={compact} />
  );

  return (
    <div className="flex min-h-screen">
      <a href="#main" className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-card focus:px-3 focus:py-1.5 focus:text-sm focus:shadow-lg">
        {t.app.skipToContent}
      </a>
      <CommandPalette />
      <aside className="sticky top-0 hidden h-screen w-56 shrink-0 flex-col border-r bg-sidebar px-3 py-5 md:flex">
        {brand}
        <div className="mt-5">
          <SearchButton />
        </div>
        <nav className="mt-3 grid gap-0.5">{nav}</nav>
        <div className="mt-auto">{userMenu(false)}</div>
      </aside>
      <div className="min-w-0 flex-1">
        <header className="sticky top-0 z-20 flex h-12 items-center gap-1 border-b bg-background/90 px-2 backdrop-blur md:hidden">
          <nav className="flex min-w-0 items-center gap-0.5">{nav}</nav>
          <div className="ml-auto flex shrink-0 items-center">
            <SearchButton compact />
            {userMenu(true)}
          </div>
        </header>
        {standby?.isStandby && (
          <div className="border-b bg-subtle">
            <div className="mx-auto flex max-w-5xl flex-wrap items-center gap-x-4 gap-y-1 px-4 py-2 text-[13px] sm:px-6 md:px-10">
              <span className="font-medium">{t.app.standby.title}</span>
              <span className="text-muted-foreground">
                {t.app.standby.readOnly}{" "}
                <a className="font-mono text-foreground underline-offset-2 hover:underline" href={`https://${standby.primaryAddress}`} target="_blank" rel="noreferrer">
                  {standby.primaryAddress}
                </a>
              </span>
              <span className="ml-auto text-muted-foreground">
                {standby.connected ? (
                  <>{standby.lastSync ? t.app.standby.syncedAgo(timeAgo(toDate(standby.lastSync))) : t.app.standby.synced}</>
                ) : (
                  <span className="text-warning">
                    {standby.lastSync ? t.app.standby.disconnectedAgo(timeAgo(toDate(standby.lastSync))) : t.app.standby.disconnected}
                  </span>
                )}
              </span>
              <PromoteButton size="xs" />
            </div>
          </div>
        )}
        <main
          id="main"
          tabIndex={-1}
          className={cn(
            "mx-auto max-w-5xl px-4 pt-8 pb-16 outline-none sm:px-6 md:px-10 md:pt-10",
            split && "xl:flex xl:max-w-none xl:items-start xl:px-0 xl:pt-0 xl:pb-0",
          )}
        >
          <ReadOnlyContext.Provider value={readOnly}>
            <Outlet />
          </ReadOnlyContext.Provider>
        </main>
      </div>
    </div>
  );
}

// NotFound：地址不存在时的页面。
export function NotFound() {
  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <Empty title={t.app.notFound.title} action={
        <Link to="/projects">
          <Button size="sm" variant="outline">{t.app.notFound.back}</Button>
        </Link>
      }>
        {t.app.notFound.body}
      </Empty>
    </div>
  );
}

// Breadcrumb：页面顶部的上级链接。
export function Breadcrumb({ items, className }: { items: { label: React.ReactNode; to?: string }[]; className?: string }) {
  return (
    <nav className={cn("mb-3 flex items-center gap-1 text-[13px] text-muted-foreground", className)}>
      {items.map((it, i) => (
        <span key={i} className="flex items-center gap-1">
          {i > 0 && <ChevronRight className="size-3.5 opacity-60" />}
          {it.to ? (
            <Link to={it.to} className="hover:text-foreground">
              {it.label}
            </Link>
          ) : (
            <span className="text-foreground">{it.label}</span>
          )}
        </span>
      ))}
    </nav>
  );
}

export function PageHeader({
  title,
  description,
  actions,
  meta,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  meta?: React.ReactNode;
}) {
  return (
    <div className="mb-8 flex flex-wrap items-start justify-between gap-4">
      <div className="min-w-0">
        <h1 className="flex flex-wrap items-center gap-3 text-2xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-1.5 text-[13px] text-pretty text-muted-foreground">{description}</p>}
        {meta && <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px] text-muted-foreground">{meta}</div>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
