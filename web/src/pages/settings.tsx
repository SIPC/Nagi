import { Fragment, useEffect, useRef } from "react";
import { PageHeader } from "@/pages/layout";
import { SecurityCard } from "@/pages/security";
import { AlertSettings } from "@/pages/alerts";
import { PanelMeshCard } from "@/pages/mesh";
import { InviteCard } from "@/pages/invite";
import { StandbyCard } from "@/pages/standby";
import { PanelAccessCard } from "@/pages/access";
import { UpdateCard, UpdateDot } from "@/pages/update";
import { Link, useSearch } from "@tanstack/react-router";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";
import { ActivityCard } from "@/pages/activity";
import { MembersCard } from "@/pages/members";
import { ProfileCard } from "@/components/account";
import { useRole } from "@/components/role";

// 设置页：左边二级菜单，右边只显示选中的一项。选中的一项记在地址的 tab 参数里。
// 没有 tab 参数时显示账号安全，所有角色都有。

export const settingsTabs = [
  { value: "security", label: t.settings.tabs.security.label },
  { value: "access", label: t.settings.tabs.access.label },
  { value: "profile", label: t.settings.tabs.profile.label },
  { value: "members", label: t.settings.tabs.members.label },
  { value: "update", label: t.settings.tabs.update.label },
  { value: "standby", label: t.settings.tabs.standby.label },
  { value: "invite", label: t.settings.tabs.invite.label },
  { value: "mesh", label: t.settings.tabs.mesh.label },
  { value: "alerts", label: t.settings.tabs.alerts.label },
  { value: "activity", label: t.settings.tabs.activity.label },
] as const;

export type SettingsTab = (typeof settingsTabs)[number]["value"];

const accountGroup = { label: t.settings.groups.account, tabs: ["profile", "security"] as SettingsTab[] };

const menuGroups = [
  accountGroup,
  { label: t.settings.groups.panel, tabs: ["access", "members", "update", "standby"] as SettingsTab[] },
  { label: t.settings.groups.nodes, tabs: ["invite", "mesh"] as SettingsTab[] },
  { label: t.settings.groups.records, tabs: ["alerts", "activity"] as SettingsTab[] },
];

// 选中项直接读地址：没有 tab 参数时显示账号安全。
export function SettingsPage(_props: { tab: SettingsTab }) {
  const tab = useSearch({ from: "/app/settings" }).tab ?? "security";
  const { isAdmin } = useRole();
  // 不是管理员时只有自己的账号。面板设置的接口也会拒绝。
  const groups = isAdmin ? menuGroups : [accountGroup];
  const allowed = groups.flatMap((g) => g.tabs);
  const cur: SettingsTab = allowed.includes(tab) ? tab : "security";
  const label = (value: SettingsTab) => settingsTabs.find((x) => x.value === value)?.label;
  // 窄屏是横向滑动的一行：选中项滚到可见。
  const navRef = useRef<HTMLElement>(null);
  useEffect(() => {
    const nav = navRef.current;
    const active = nav?.querySelector<HTMLElement>('[aria-current="page"]');
    if (!nav || !active) return;
    nav.scrollTo({ left: active.getBoundingClientRect().left - nav.getBoundingClientRect().left + nav.scrollLeft - (nav.clientWidth - active.offsetWidth) / 2 });
  }, [cur]);
  return (
    <>
      <PageHeader title={t.settings.title} />
      <div className="grid gap-6 md:grid-cols-[180px_minmax(0,1fr)] md:gap-12">
        {/* 窄屏是一行可以横向滑动的标签，分组标题收起。 */}
        <nav ref={navRef} aria-label={t.settings.title} className="flex gap-1 overflow-x-auto [scrollbar-width:none] md:grid md:content-start md:overflow-visible">
          {groups.map((g) => (
            <Fragment key={g.label}>
              <div className="mt-2 hidden h-6 items-center px-2 text-xs leading-5 font-medium text-muted-foreground first:mt-0 md:flex">{g.label}</div>
              {g.tabs.map((value) => (
                <Link
                  key={value}
                  to="/settings"
                  search={{ tab: value === "security" ? undefined : value }}
                  replace
                  activeOptions={{ explicitUndefined: true }}
                  aria-current={value === cur ? "page" : undefined}
                  className={cn(
                    "flex h-8 shrink-0 cursor-pointer items-center gap-2 rounded-md px-2 text-left text-sm outline-none transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/40 focus-visible:ring-inset max-md:h-10",
                    value === cur && "bg-accent",
                  )}
                >
                  {label(value)}
                  {value === "update" && <UpdateDot />}
                </Link>
              ))}
            </Fragment>
          ))}
        </nav>
        <div className="min-w-0">
          {cur === "profile" && <ProfileCard />}
          {cur === "access" && <PanelAccessCard />}
          {cur === "security" && <SecurityCard />}
          {cur === "members" && <MembersCard />}
          {cur === "alerts" && <AlertSettings />}
          {cur === "activity" && <ActivityCard />}
          {cur === "standby" && <StandbyCard />}
          {cur === "invite" && <InviteCard />}
          {cur === "mesh" && <PanelMeshCard />}
          {cur === "update" && <UpdateCard />}
        </div>
      </div>
    </>
  );
}
