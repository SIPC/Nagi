import { createContext, useContext, useEffect, useState } from "react";
import { createPortal, flushSync } from "react-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useMatch, useNavigate, useRouterState } from "@tanstack/react-router";
import { Box, ChevronLeft, ChevronRight, Menu as MenuIcon, ScrollText, Server, SlidersHorizontal, X } from "lucide-react";
import { Dialog as DialogPrimitive } from "radix-ui";
import { authClient, nodeClient, projectClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { ActiveBox, Button, ButtonLink, ReadOnlyContext, ReadOnlyReasonContext, Slide, useActiveBox, useIsMobile, Waves } from "@/components/ui";
import { PromoteButton, useStandbys } from "@/pages/standby";
import { UpdateDot, UpdateTips } from "@/pages/update";
import { UserMenu, type ThemeMode } from "@/components/account";
import { t } from "@/i18n";
import { CommandPalette, SearchButton } from "@/components/command-palette";
import { settingsTabsFor } from "@/pages/settings";
import type { Node } from "@/gen/nagi/v1/api_pb";
import { useRole } from "@/components/role";
import { MobileProjectNav } from "@/pages/project";
import { PanelDown } from "@/components/panel-down";
import { forgetStandbys, registerOfflinePage, rememberStandbys } from "@/lib/offline";

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
    const apply = () => {
      const dark = mode === "dark" || (mode === "system" && media.matches);
      document.documentElement.classList.toggle("dark", dark);
      document.querySelector('meta[name="theme-color"]')?.setAttribute("content", dark ? "#0b0c0d" : "#ffffff");
    };
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
    const apply = () => flushSync(() => setMode(next));
    if (document.startViewTransition && !matchMedia("(prefers-reduced-motion: reduce)").matches) document.startViewTransition(apply);
    else apply();
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
  // bar：手机标题栏上的字。项目里是项目名（灰字），其他是当前页。
  let bar: string | undefined;
  if (parts[0] === "overview") title = t.app.nav.overview;
  else if (parts[0] === "activity") title = t.app.nav.activity;
  else if (parts[0] === "projects" && parts[1] === "new") title = t.app.search.newProject;
  else if (projectId) {
    const name = project.data?.project?.name ?? t.app.nav.projects;
    bar = project.data?.project?.name ?? "";
    const container = parts[2] === "c" && parts[3] ? decodeURIComponent(parts[3]) : "";
    const display = project.data?.project?.containers.find((c) => c.name === container)?.displayName || container;
    title = container ? `${display} · ${name}` : name;
  } else if (parts[0] === "nodes") {
    title = parts[1] ? (nodes?.find((n) => n.id === parts[1])?.name ?? t.app.nav.nodes) : t.app.nav.nodes;
  } else if (parts[0] === "settings") {
    const allowed = settingsTabsFor(isAdmin);
    const tab = allowed.find((x) => x.value === (location.search as { tab?: string }).tab) ?? allowed.find((x) => x.value === "security") ?? allowed[0];
    title = `${tab.label} · ${t.app.nav.settings}`;
  }
  useEffect(() => {
    document.title = `${title} · Nagi`;
    return () => {
      document.title = "Nagi";
    };
  }, [title]);
  // 新建项目是项目列表上的对话框。
  const section = parts[0] === "settings" ? t.app.nav.settings : parts[1] === "new" ? t.app.nav.projects : title;
  return { projectId, inProject: bar !== undefined, barTitle: bar ?? section };
}

// 手机标题栏（高 56）。默认是 ☰、当前页、搜索；文件页、编辑页、添加容器这些子页面用 MobileBar 换成自己的。
// MobileBar 用 portal 放进标题栏，所以按钮还在页面的 React 树里；放进去后默认的那一排由 CSS 隐藏，不用来回传状态。
const MobileBarSlot = createContext<HTMLElement | null>(null);

export function MobileBar({ back, left, title, right }: { back?: () => void; left?: React.ReactNode; title: React.ReactNode; right?: React.ReactNode }) {
  const slot = useContext(MobileBarSlot);
  if (!slot) return null;
  return createPortal(
    <div data-mobile-bar className="flex h-full items-center gap-2">
      {back ? (
        <Button readOnlyOk variant="ghost" size="icon" aria-label={t.common.actions.back} onClick={back}>
          <ChevronLeft />
        </Button>
      ) : (
        left
      )}
      <span className="min-w-0 flex-1 truncate text-center font-medium">{title}</span>
      {right ?? <span className="w-10 shrink-0" />}
    </div>,
    slot,
  );
}

// 手机标题栏里的文字按钮：取消、保存。
export const mobileBarText = "px-2 text-base";

// MobileNav：☰ 打开的全屏导航。全局时是四个入口和用户菜单；项目里是这个项目的导航。
function MobileNav({ open, onClose, title, children }: { open: boolean; onClose: () => void; title: string; children: React.ReactNode }) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Content aria-describedby={undefined} className="fixed inset-0 z-40 flex flex-col overflow-y-auto bg-background outline-none data-[state=open]:animate-fade-in data-[state=closed]:animate-fade-out">
          <div className="flex h-14 shrink-0 items-center gap-2 border-b px-2">
            <DialogPrimitive.Close asChild>
              <Button readOnlyOk variant="ghost" size="icon" aria-label={t.app.nav.close}>
                <X />
              </Button>
            </DialogPrimitive.Close>
            <DialogPrimitive.Title className="min-w-0 flex-1 truncate font-medium">{title}</DialogPrimitive.Title>
          </div>
          {/* 点了链接就关：包括当前页的链接，这时地址不变。 */}
          <div className="flex flex-1 flex-col px-4 py-6" onClick={(e) => (e.target as Element).closest("a") && onClose()}>
            {children}
          </div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

function OverviewIcon() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M1.5 8h3l2-5 3 10 2-5h3" />
    </svg>
  );
}

// 左侧导航（参照 OpenAI 平台后台）：上面是产品名和四个入口，下面是用户卡片。手机上收进 ☰ 打开的全屏导航。
const navItems = [
  { to: "/overview", label: t.app.nav.overview, icon: OverviewIcon },
  { to: "/projects", label: t.app.nav.projects, icon: Box },
  { to: "/nodes", label: t.app.nav.nodes, icon: Server },
  // 活动只给管理员。
  { to: "/activity", label: t.app.nav.activity, icon: ScrollText, adminOnly: true },
  { to: "/settings", label: t.app.nav.settings, icon: SlidersHorizontal },
] as const;

const navLink =
  "relative flex h-8 items-center gap-2 rounded-md px-2 text-sm transition-colors not-data-active:hover:bg-accent/50 [&_svg]:size-4";
const mobileNavLink = cn(navLink.replace("h-8", "h-11"), "data-[status=active]:bg-accent");

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
  const page = usePageTitle(nodes.data?.nodes, isAdmin);
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const href = useRouterState({ select: (s) => s.location.href });
  const mobile = useIsMobile();
  const [navOpen, setNavOpen] = useState(false);
  const [barSlot, setBarSlot] = useState<HTMLElement | null>(null);
  // 换了页面（例如用户菜单里的「安全」）就收起导航。
  useEffect(() => setNavOpen(false), [href]);
  // 备用面板全部只读。账号角色：只读角色的项目和节点只能看，操作员的节点只能看；设置页里只有自己的账号，不受影响。
  const readOnly = (standby?.isStandby ?? false) || (!pathname.startsWith("/settings") && !pathname.startsWith("/activity") && (isViewer || (pathname.startsWith("/nodes") && !isAdmin)));
  // 项目页（含容器页）宽屏取消内容区的宽度限制，由 ProjectSplit 分成项目栏和右边的内容。
  const split = !!useMatch({ from: "/app/projects/$projectId", shouldThrow: false });
  const navBox = useActiveBox();

  // 面板所在机器挂了时提示转到备用面板（见 docs/standby.md#面板不可用时）：记下备用面板的地址，注册离线页。
  useEffect(registerOfflinePage, []);
  const standbyAddresses = standby && !standby.isStandby ? JSON.stringify(standby.standbys.map((s) => ({ name: s.name, address: s.address }))) : undefined;
  useEffect(() => {
    if (standbyAddresses !== undefined) rememberStandbys(JSON.parse(standbyAddresses));
  }, [standbyAddresses]);

  const logout = async () => {
    await authClient.logout({});
    forgetStandbys();
    queryClient.clear();
    navigate({ to: "/login" });
  };

  const brand = (
    <Link to="/overview" aria-label="Nagi" className="flex h-8 items-center px-2">
      <Waves className="h-4 w-7 text-wave" />
    </Link>
  );
  const nav = (className: string) => navItems.filter((it) => isAdmin || !("adminOnly" in it)).map((it) => (
    <Link key={it.to} to={it.to} data-active={pathname.startsWith(it.to) || undefined} className={className}>
      <it.icon />
      {it.label}
      {it.to === "/settings" && <UpdateDot />}
      {it.to === "/nodes" && offlineCount > 0 && (
        <span className="ml-auto text-xs text-destructive" title={t.app.nodesOffline(offlineCount)}>
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
      <CommandPalette canCreateProject={!(standby?.isStandby ?? false) && !isViewer} />
      {me.data?.role === "admin" && <UpdateTips nodes={nodes.data?.nodes ?? []} isStandby={standby?.isStandby ?? false} />}
      <aside className="sticky top-0 hidden h-screen w-60 shrink-0 flex-col border-r bg-sidebar px-3 pt-4 pb-3 md:flex">
        {brand}
        <div className="mt-4">
          <SearchButton />
        </div>
        <nav ref={navBox.ref} className="relative mt-4 grid gap-1">
          <ActiveBox box={navBox.box} />
          {nav(navLink)}
        </nav>
        <div className="mt-auto">{userMenu(false)}</div>
      </aside>
      <div className="min-w-0 flex-1">
        <header className="group/bar material sticky top-0 z-20 h-14 border-b px-2 [--material-base:var(--background)] md:hidden">
          <div className="flex h-full items-center gap-2 group-has-data-mobile-bar/bar:hidden">
            <Button readOnlyOk variant="ghost" size="icon" aria-label={t.app.nav.open} aria-expanded={navOpen} onClick={() => setNavOpen(true)}>
              <MenuIcon />
            </Button>
            <span className={cn("min-w-0 flex-1 truncate", !page.inProject && "font-medium", page.inProject && "text-muted-foreground")}>{page.barTitle}</span>
            <SearchButton compact />
          </div>
          <div ref={setBarSlot} className="contents" />
        </header>
        {mobile && (
          <MobileNav open={navOpen} onClose={() => setNavOpen(false)} title={page.barTitle}>
            {page.inProject ? (
              <MobileProjectNav projectId={page.projectId} />
            ) : (
              <>
                <nav className="grid gap-1">{nav(mobileNavLink)}</nav>
                <div className="mt-auto pt-4 [&>button]:h-11">{userMenu(false)}</div>
              </>
            )}
          </MobileNav>
        )}
        {standby?.isStandby && (
          <div className="border-b">
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-2 text-[13px] md:px-8">
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
                  <span className="text-warning-text">
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
            "mx-auto max-w-[1184px] overflow-x-clip px-4 pt-6 pb-16 outline-none md:px-8 md:pt-8",
            split && "max-md:pt-4",
            split && "xl:flex xl:max-w-none xl:items-start xl:px-0 xl:pt-0 xl:pb-0",
          )}
        >
          <ReadOnlyContext.Provider value={readOnly}>
            <ReadOnlyReasonContext.Provider value={standby?.isStandby ? "standby" : "role"}>
              <MobileBarSlot.Provider value={barSlot}>
                {/* 项目页由 ProjectSplit 分栏，这一层不占位，内容的动画也由它管。 */}
                <Slide id={split ? "split" : pathname} index={navItems.findIndex((it) => pathname.startsWith(it.to))} variant="page" className={split ? "contents" : undefined}>
                  <Outlet />
                </Slide>
              </MobileBarSlot.Provider>
            </ReadOnlyReasonContext.Provider>
          </ReadOnlyContext.Provider>
        </main>
      </div>
      <PanelDown />
    </div>
  );
}

// NotFound：地址不存在时的页面。
export function NotFound() {
  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4 py-8">
      <main className="w-full max-w-[360px]">
        <div className="grid justify-items-center gap-2 text-center">
          <svg viewBox="4 9 24 14" fill="none" aria-hidden className="h-6 w-[42px]">
            <path d="M6 19c3-3 6-3 10 0s7 3 10 0" stroke="var(--wave)" strokeWidth="2.4" strokeLinecap="round" />
            <path d="M6 13c3-3 6-3 10 0s7 3 10 0" stroke="var(--muted-foreground)" strokeWidth="2.4" strokeLinecap="round" />
          </svg>
          <h1 className="mt-2 text-2xl leading-8 font-semibold">{t.app.notFound.title}</h1>
          <p className="text-muted-foreground">{t.app.notFound.body}</p>
          <ButtonLink to="/projects" className="mt-4" readOnlyOk>
            {t.app.notFound.back}
          </ButtonLink>
        </div>
      </main>
    </div>
  );
}

// Breadcrumb：页面顶部的上级链接。
export function Breadcrumb({ items, className }: { items: { label: React.ReactNode; to?: string }[]; className?: string }) {
  return (
    <nav className={cn("mb-2 flex items-center gap-1 text-[13px] leading-5 text-muted-foreground", className)}>
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
  className,
  actionsClassName,
}: {
  className?: string;
  actionsClassName?: string;
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  meta?: React.ReactNode;
}) {
  return (
    <div className={cn("mb-6 flex flex-wrap items-start justify-between gap-4", className)}>
      <div className="min-w-0">
        <h1 className="flex flex-wrap items-center gap-3 text-2xl leading-8 font-semibold tracking-tight [&>span]:font-normal [&>span]:tracking-normal">{title}</h1>
        {description && <p className="text-pretty text-muted-foreground">{description}</p>}
        {meta && <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-muted-foreground">{meta}</div>}
      </div>
      {actions && <div className={cn("flex flex-wrap items-center gap-2", actionsClassName)}>{actions}</div>}
    </div>
  );
}
