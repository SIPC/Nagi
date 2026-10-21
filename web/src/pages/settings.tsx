import { PageHeader } from "@/pages/layout";
import { SecurityCard } from "@/pages/security";
import { AlertSettings } from "@/pages/alerts";
import { PanelMeshCard } from "@/pages/mesh";
import { InviteCard } from "@/pages/invite";
import { StandbyCard } from "@/pages/standby";
import { PanelAccessCard } from "@/pages/access";
import { cn } from "@/lib/utils";

// 设置页：左边二级菜单，右边只显示选中的一组（参照 OpenAI 平台、Render 的设置页）。选中的一组记在地址的 tab 参数里。

export const settingsTabs = [
  { value: "access", label: "面板访问", description: "节点和浏览器访问本面板用的地址。" },
  { value: "security", label: "账号安全", description: "登录密码、两步验证和安全入口。" },
  { value: "alerts", label: "告警", description: "节点和容器异常时的通知渠道和触发规则。" },
  { value: "standby", label: "备用面板", description: "实时同步本面板数据的备用面板，主面板故障时提升为主面板。" },
  { value: "invite", label: "接收移交", description: "别的面板把节点移交给本面板时使用的接入码。" },
  { value: "mesh", label: "Mesh", description: "面板在 mesh 网络中的地址和与各节点的连接。" },
] as const;

export type SettingsTab = (typeof settingsTabs)[number]["value"];

export function SettingsPage({ tab, onTab }: { tab: SettingsTab; onTab: (t: SettingsTab) => void }) {
  const cur = settingsTabs.find((t) => t.value === tab) ?? settingsTabs[0];
  return (
    <>
      <PageHeader title="设置" />
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
