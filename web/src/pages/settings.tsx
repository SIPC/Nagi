import { PageHeader } from "@/pages/layout";
import { SecurityCard } from "@/pages/security";
import { AlertSettings } from "@/pages/alerts";
import { PanelMeshCard } from "@/pages/mesh";
import { InviteCard } from "@/pages/invite";
import { StandbyCard } from "@/pages/standby";
import { PanelAccessCard } from "@/pages/access";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";

// 设置页：左边二级菜单，右边只显示选中的一组（参照 OpenAI 平台、Render 的设置页）。选中的一组记在地址的 tab 参数里。

export const settingsTabs = [
  { value: "access", label: t.settings.tabs.access.label, description: t.settings.tabs.access.description },
  { value: "security", label: t.settings.tabs.security.label, description: t.settings.tabs.security.description },
  { value: "alerts", label: t.settings.tabs.alerts.label, description: t.settings.tabs.alerts.description },
  { value: "standby", label: t.settings.tabs.standby.label, description: t.settings.tabs.standby.description },
  { value: "invite", label: t.settings.tabs.invite.label, description: t.settings.tabs.invite.description },
  { value: "mesh", label: t.settings.tabs.mesh.label, description: t.settings.tabs.mesh.description },
] as const;

export type SettingsTab = (typeof settingsTabs)[number]["value"];

export function SettingsPage({ tab, onTab }: { tab: SettingsTab; onTab: (t: SettingsTab) => void }) {
  const cur = settingsTabs.find((t) => t.value === tab) ?? settingsTabs[0];
  return (
    <>
      <PageHeader title={t.settings.title} />
      <div className="grid gap-8 md:grid-cols-[11rem_minmax(0,1fr)] md:gap-12">
        <nav className="flex gap-1 overflow-x-auto md:flex-col md:overflow-visible">
          {settingsTabs.map((t) => (
            <button
              key={t.value}
              type="button"
              onClick={() => onTab(t.value)}
              className={cn(
                "flex h-8 shrink-0 cursor-pointer items-center rounded-md px-2.5 text-left text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground",
                t.value === cur.value && "bg-accent font-medium text-foreground",
              )}
            >
              {t.label}
            </button>
          ))}
        </nav>
        <div className="min-w-0">
          <div className="mb-6 border-b pb-5">
            <h2 className="text-lg font-semibold">{cur.label}</h2>
            <p className="mt-1 text-[13px] text-muted-foreground">{cur.description}</p>
          </div>
          {cur.value === "access" && <PanelAccessCard />}
          {cur.value === "security" && <SecurityCard />}
          {cur.value === "alerts" && <AlertSettings />}
          {cur.value === "standby" && <StandbyCard />}
          {cur.value === "invite" && <InviteCard />}
          {cur.value === "mesh" && <PanelMeshCard />}
        </div>
      </div>
    </>
  );
}
