import { Fragment, useEffect } from "react";
import { PageHeader } from "@/pages/layout";
import { SecurityCard } from "@/pages/security";
import { AlertSettings } from "@/pages/alerts";
import { PanelMeshCard } from "@/pages/mesh";
import { InviteCard } from "@/pages/invite";
import { StandbyCard } from "@/pages/standby";
import { PanelAccessCard } from "@/pages/access";
import { UpdateDot } from "@/pages/update";
import { AboutCard } from "@/pages/about";
import { Link, useSearch } from "@tanstack/react-router";
import { cn } from "@/lib/utils";
import { ActiveBox, Slide, useActiveBox } from "@/components/ui";
import { t } from "@/i18n";
import { MembersCard } from "@/pages/members";
import { ProfileCard } from "@/components/account";
import { useRole } from "@/components/role";

// 设置页：左边二级菜单，右边只显示选中的一项。选中的一项记在地址的 tab 参数里。
// 没有 tab 参数时显示菜单第一项个人资料，所有角色都有。

export const settingsTabs = [
  { value: "security", label: t.settings.tabs.security.label },
  { value: "access", label: t.settings.tabs.access.label },
  { value: "profile", label: t.settings.tabs.profile.label },
  { value: "members", label: t.settings.tabs.members.label },
  { value: "standby", label: t.settings.tabs.standby.label },
  { value: "invite", label: t.settings.tabs.invite.label },
  { value: "mesh", label: t.settings.tabs.mesh.label },
  { value: "alerts", label: t.settings.tabs.alerts.label },
  { value: "about", label: t.settings.tabs.about.label },
] as const;

export type SettingsTab = (typeof settingsTabs)[number]["value"];

const accountGroup = { label: t.settings.groups.account, tabs: ["profile", "security"] as SettingsTab[] };

// 只有一项，不要分组标题（和菜单项重复），只留分组之间的间距。
const aboutGroup = { label: "", tabs: ["about"] as SettingsTab[] };

const menuGroups = [
  accountGroup,
  { label: t.settings.groups.panel, tabs: ["access", "members", "alerts", "standby"] as SettingsTab[] },
  { label: t.settings.groups.nodes, tabs: ["invite", "mesh"] as SettingsTab[] },
  aboutGroup,
];

// 不是管理员时只有自己的账号和关于。面板设置的接口也会拒绝。
function groupsFor(isAdmin: boolean) {
  return isAdmin ? menuGroups : [accountGroup, aboutGroup];
}

// settingsTabsFor：当前角色能看到的设置项，按菜单顺序。标签页标题、命令面板也用它。
export function settingsTabsFor(isAdmin: boolean) {
  const values = groupsFor(isAdmin).flatMap((g) => g.tabs);
  return values.map((value) => settingsTabs.find((x) => x.value === value)!);
}

// 选中项直接读地址：没有 tab 参数（或没有权限看）时显示菜单第一项。
export function SettingsPage() {
  const { isAdmin } = useRole();
  const groups = groupsFor(isAdmin);
  const allowed = groups.flatMap((g) => g.tabs);
  const tab = useSearch({ from: "/app/settings" }).tab ?? allowed[0];
  const cur: SettingsTab = allowed.includes(tab) ? tab : allowed[0];
  const label = (value: SettingsTab) => settingsTabs.find((x) => x.value === value)?.label;
  // 窄屏是横向滑动的一行：选中项滚到可见。
  const { ref: navRef, box } = useActiveBox();
  useEffect(() => {
    const nav = navRef.current;
    const active = nav?.querySelector<HTMLElement>('[aria-current="page"]');
    if (!nav || !active) return;
    nav.scrollTo({ left: active.getBoundingClientRect().left - nav.getBoundingClientRect().left + nav.scrollLeft - (nav.clientWidth - active.offsetWidth) / 2 });
  }, [cur, navRef]);
  return (
    <>
      <PageHeader title={t.settings.title} />
      <div className="grid gap-6 md:grid-cols-[180px_minmax(0,1fr)] md:gap-12">
        {/* 窄屏是一行可以横向滑动的标签，分组标题收起。 */}
        <nav ref={navRef} aria-label={t.settings.title} className="relative flex gap-1 overflow-x-auto [scrollbar-width:none] md:grid md:content-start md:self-start md:overflow-visible">
          <ActiveBox box={box} className="max-md:hidden" />
          {groups.map((g) => (
            <Fragment key={g.tabs[0]}>
              {/* 分组标题和项目栏顶部的「项目」一样，13 号次文字。 */}
              {g.label ? <div className="relative mt-2 hidden h-6 items-center px-2 text-[13px] leading-5 text-muted-foreground first:mt-0 md:flex">{g.label}</div> : <div className="hidden h-2 md:block" />}
              {g.tabs.map((value) => (
                <Link
                  key={value}
                  to="/settings"
                  // 切换子页是一次导航，后退回到上一个子页，不直接离开设置页。
                  search={{ tab: value === allowed[0] ? undefined : value }}
                  activeOptions={{ explicitUndefined: true }}
                  aria-current={value === cur ? "page" : undefined}
                  data-active={value === cur || undefined}
                  className={cn(
                    "relative flex h-8 shrink-0 cursor-pointer items-center gap-2 rounded-md px-2 text-left text-sm outline-none transition-colors not-data-active:hover:bg-accent/50 focus-ring focus-visible:ring-inset max-md:h-10",
                    value === cur && "max-md:bg-accent",
                  )}
                >
                  {label(value)}
                  {value === "about" && <UpdateDot />}
                </Link>
              ))}
            </Fragment>
          ))}
        </nav>
        <Slide id={cur} index={allowed.indexOf(cur)} variant="settings" className="min-w-0">
          {cur === "profile" && <ProfileCard />}
          {cur === "access" && <PanelAccessCard />}
          {cur === "security" && <SecurityCard />}
          {cur === "members" && <MembersCard />}
          {cur === "alerts" && <AlertSettings />}
          {cur === "standby" && <StandbyCard />}
          {cur === "invite" && <InviteCard />}
          {cur === "mesh" && <PanelMeshCard />}
          {cur === "about" && <AboutCard />}
        </Slide>
      </div>
    </>
  );
}
