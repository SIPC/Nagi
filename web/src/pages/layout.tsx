import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useMatch, useNavigate } from "@tanstack/react-router";
import { Boxes, ChevronRight, LogOut, Moon, Server, Settings, Sun } from "lucide-react";
import { authClient, toDate } from "@/lib/api";
import { cn, timeAgo } from "@/lib/utils";
import { Button, ReadOnlyContext, Waves } from "@/components/ui";
import { PromoteButton, useStandbys } from "@/pages/standby";
import { LanguageButton } from "@/components/language";
import { t } from "@/i18n";

function useTheme() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  const toggle = () => {
    const next = !dark;
    document.documentElement.classList.toggle("dark", next);
    try {
      localStorage.setItem("theme", next ? "dark" : "light");
    } catch {
      // 隐私模式等情况下无法保存，不影响切换。
    }
    setDark(next);
  };
  return { dark, toggle };
}

// 左侧导航（参照 OpenAI 平台后台）：上面是产品名和三个入口，下面是账号、主题和退出。窄屏时收成顶部一行。
const navItems = [
  { to: "/projects", label: t.app.nav.projects, icon: Boxes },
  { to: "/nodes", label: t.app.nav.nodes, icon: Server },
  { to: "/settings", label: t.app.nav.settings, icon: Settings },
] as const;

const navLink =
  "flex h-8 items-center gap-2.5 rounded-md px-2.5 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground data-[status=active]:bg-accent data-[status=active]:font-medium data-[status=active]:text-foreground [&_svg]:size-4";

export function AppLayout() {
  const { dark, toggle } = useTheme();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const me = useQuery({ queryKey: ["me"], queryFn: () => authClient.me({}) });
  const standby = useStandbys().data;
  const readOnly = standby?.isStandby ?? false;
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
    </Link>
  ));
  const actions = (
    <>
      <LanguageButton account />
      <Button
        readOnlyOk
        variant="ghost"
        size="icon"
        onClick={toggle}
        aria-label={dark ? t.app.theme.toLight : t.app.theme.toDark}
        title={dark ? t.app.theme.light : t.app.theme.dark}
      >
        {dark ? <Sun /> : <Moon />}
      </Button>
      <Button readOnlyOk variant="ghost" size="icon" onClick={logout} aria-label={t.app.logout} title={t.app.logout}>
        <LogOut />
      </Button>
    </>
  );

  return (
    <div className="flex min-h-screen">
      <aside className="sticky top-0 hidden h-screen w-56 shrink-0 flex-col border-r bg-sidebar px-3 py-5 md:flex">
        {brand}
        <nav className="mt-6 grid gap-0.5">{nav}</nav>
        <div className="mt-auto flex items-center gap-1">
          <span className="min-w-0 flex-1 truncate px-2.5 text-[13px] text-muted-foreground">{me.data?.username}</span>
          {actions}
        </div>
      </aside>
      <div className="min-w-0 flex-1">
        <header className="sticky top-0 z-20 flex h-12 items-center gap-1 border-b bg-background/90 px-2 backdrop-blur md:hidden">
          <nav className="flex min-w-0 items-center gap-0.5">{nav}</nav>
          <div className="ml-auto flex shrink-0 items-center">{actions}</div>
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
          className={cn(
            "mx-auto max-w-5xl px-4 pt-8 pb-16 sm:px-6 md:px-10 md:pt-10",
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
