import { PageHeader } from "@/pages/layout";
import { SecurityCard } from "@/pages/security";
import { AlertSettings } from "@/pages/alerts";
import { PanelMeshCard } from "@/pages/mesh";
import { InviteCard } from "@/pages/invite";
import { StandbyCard } from "@/pages/standby";
import { PanelAccessCard } from "@/pages/access";
import { UpdateCard, UpdateDot } from "@/pages/update";
import { Link } from "@tanstack/react-router";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";
import { ActivityCard } from "@/pages/activity";
import { MembersCard } from "@/pages/members";
import { useRole } from "@/components/role";

// 设置页：左边二级菜单，右边只显示选中的一组（参照 OpenAI 平台、Render 的设置页）。选中的一组记在地址的 tab 参数里。

export const settingsTabs = [
  { value: "access", label: t.settings.tabs.access.label, description: t.settings.tabs.access.description },
  { value: "security", label: t.settings.tabs.security.label, description: t.settings.tabs.security.description },
  { value: "members", label: t.settings.tabs.members.label, description: t.settings.tabs.members.description },
  { value: "alerts", label: t.settings.tabs.alerts.label, description: t.settings.tabs.alerts.description },
  { value: "activity", label: t.settings.tabs.activity.label, description: t.settings.tabs.activity.description },
  { value: "standby", label: t.settings.tabs.standby.label, description: t.settings.tabs.standby.description },
  { value: "invite", label: t.settings.tabs.invite.label, description: t.settings.tabs.invite.description },
  { value: "mesh", label: t.settings.tabs.mesh.label, description: t.settings.tabs.mesh.description },
  { value: "update", label: t.settings.tabs.update.label, description: t.settings.tabs.update.description },
] as const;

export type SettingsTab = (typeof settingsTabs)[number]["value"];

export function SettingsPage({ tab }: { tab: SettingsTab }) {
  const { isAdmin } = useRole();
  // 不是管理员时只有自己的账号。面板设置的接口也会拒绝。
  const tabs = isAdmin ? [...settingsTabs] : settingsTabs.filter((x) => x.value === "security");
  const cur = tabs.find((t) => t.value === tab) ?? tabs[0];
  return (
    <>
      <PageHeader title={t.settings.title} />
      <div className="grid gap-8 md:grid-cols-[11rem_minmax(0,1fr)] md:gap-12">
        <nav className="flex gap-1 overflow-x-auto md:flex-col md:overflow-visible">
          {tabs.map((t) => (
            <Link
              key={t.value}
              to="/settings"
              search={{ tab: t.value === "access" ? undefined : t.value }}
              replace
              activeOptions={{ explicitUndefined: true }}
              aria-current={t.value === cur.value ? "page" : undefined}
              className={cn(
                "flex h-8 shrink-0 cursor-pointer items-center rounded-md px-2.5 text-left text-sm text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40 focus-visible:ring-inset",
                t.value === cur.value && "bg-accent font-medium text-foreground",
              )}
            >
              {t.label}
              {t.value === "update" && <UpdateDot />}
            </Link>
          ))}
        </nav>
        <div className="min-w-0">
          <div className="mb-6 border-b pb-5">
            <h2 className="text-lg font-semibold">{cur.label}</h2>
            <p className="mt-1 text-[13px] text-muted-foreground">{cur.description}</p>
          </div>
          {cur.value === "access" && <PanelAccessCard />}
          {cur.value === "security" && <SecurityCard />}
          {cur.value === "members" && <MembersCard />}
          {cur.value === "alerts" && <AlertSettings />}
          {cur.value === "activity" && <ActivityCard />}
          {cur.value === "standby" && <StandbyCard />}
          {cur.value === "invite" && <InviteCard />}
          {cur.value === "mesh" && <PanelMeshCard />}
          {cur.value === "update" && <UpdateCard />}
        </div>
      </div>
    </>
  );
}
