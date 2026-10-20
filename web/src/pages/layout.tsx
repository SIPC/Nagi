import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { ChevronRight, LogOut, Moon, Sun } from "lucide-react";
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

const navLink =
  "relative flex h-14 items-center px-1 text-sm text-muted-foreground transition-colors hover:text-foreground data-[status=active]:text-foreground data-[status=active]:after:absolute data-[status=active]:after:inset-x-0 data-[status=active]:after:bottom-0 data-[status=active]:after:h-0.5 data-[status=active]:after:bg-foreground";

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

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-20 border-b bg-background/85 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-8 px-6">
          <Link to="/projects" className="flex items-center gap-2 text-[15px] font-semibold tracking-tight">
            <Waves className="h-3.5 w-8 text-wave" />
            Nagi
          </Link>
          <nav className="flex items-center gap-6">
            <Link to="/projects" className={navLink}>
              项目
            </Link>
            <Link to="/nodes" className={navLink}>
              节点
            </Link>
            <Link to="/settings" className={navLink}>
              设置
            </Link>
          </nav>
          <div className="ml-auto flex items-center gap-1">
            {me.data?.username && <span className="mr-2 hidden text-[13px] text-muted-foreground sm:inline">{me.data.username}</span>}
            <Button readOnlyOk variant="ghost" size="icon" onClick={toggle} aria-label={dark ? "切换为浅色" : "切换为深色"} title={dark ? "浅色" : "深色"}>
              {dark ? <Sun /> : <Moon />}
            </Button>
            <Button readOnlyOk variant="ghost" size="icon" onClick={logout} aria-label="退出登录" title="退出登录">
              <LogOut />
            </Button>
          </div>
        </div>
      </header>
      {standby?.isStandby && (
        <div className="border-b bg-muted/60">
          <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-x-4 gap-y-1 px-6 py-2 text-[13px]">
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
      <main className="mx-auto max-w-6xl px-6 pt-8 pb-16">
        <ReadOnlyContext.Provider value={readOnly}>
          <Outlet />
        </ReadOnlyContext.Provider>
      </main>
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
    <div className="mb-6 flex flex-wrap items-start justify-between gap-4">
      <div className="min-w-0">
        <h1 className="flex flex-wrap items-center gap-3 text-xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-1 text-[13px] text-muted-foreground">{description}</p>}
        {meta && <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px] text-muted-foreground">{meta}</div>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
