import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { Boxes, ChevronRight, LogOut, Moon, Server, Settings, Sun } from "lucide-react";
import { authClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
import { Button, ReadOnlyContext, Waves } from "@/components/ui";
import { PromoteButton, useStandbys } from "@/pages/standby";

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
  { to: "/projects", label: "项目", icon: Boxes },
  { to: "/nodes", label: "节点", icon: Server },
  { to: "/settings", label: "设置", icon: Settings },
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
      <Button readOnlyOk variant="ghost" size="icon" onClick={toggle} aria-label={dark ? "切换为浅色" : "切换为深色"} title={dark ? "浅色" : "深色"}>
        {dark ? <Sun /> : <Moon />}
      </Button>
      <Button readOnlyOk variant="ghost" size="icon" onClick={logout} aria-label="退出登录" title="退出登录">
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
            <div className="mx-auto flex max-w-5xl flex-wrap items-center gap-x-4 gap-y-1 px-6 py-2 text-[13px] md:px-10">
              <span className="font-medium">备用面板</span>
              <span className="text-muted-foreground">
                只能查看，修改请到主面板{" "}
                <a className="font-mono text-foreground underline-offset-2 hover:underline" href={`https://${standby.primaryAddress}`} target="_blank" rel="noreferrer">
                  {standby.primaryAddress}
                </a>
              </span>
              <span className="ml-auto text-muted-foreground">
                {standby.connected ? (
                  <>已同步{standby.lastSync && `（${timeAgo(toDate(standby.lastSync))}）`}</>
                ) : (
                  <span className="text-warning">未连接主面板{standby.lastSync && `，最近同步 ${timeAgo(toDate(standby.lastSync))}`}</span>
                )}
              </span>
              <PromoteButton size="xs" />
            </div>
          </div>
        )}
        <main className="mx-auto max-w-5xl px-6 pt-8 pb-16 md:px-10 md:pt-10">
          <ReadOnlyContext.Provider value={readOnly}>
            <Outlet />
          </ReadOnlyContext.Provider>
        </main>
      </div>
    </div>
  );
}

// Breadcrumb：页面顶部的上级链接。
export function Breadcrumb({ items }: { items: { label: React.ReactNode; to?: string }[] }) {
  return (
    <nav className="mb-3 flex items-center gap-1 text-[13px] text-muted-foreground">
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
        {description && <p className="mt-1 text-[13px] text-muted-foreground">{description}</p>}
        {meta && <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px] text-muted-foreground">{meta}</div>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
