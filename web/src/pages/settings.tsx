import { PageHeader } from "@/pages/layout";
import { SecurityCard } from "@/pages/security";
import { AlertSettings } from "@/pages/alerts";
import { PanelMeshCard } from "@/pages/mesh";
import { InviteCard } from "@/pages/invite";
import { StandbyCard } from "@/pages/standby";
import { PanelAccessCard } from "@/pages/access";

// 面板自己的设置：访问地址、账号安全、告警、备用面板、接入码（让别的面板把节点交给本面板）、面板的 mesh。
export function SettingsPage() {
  return (
    <>
      <PageHeader title="设置" description="面板自身的访问地址、账号安全、告警和备用面板；添加节点请到节点页。" />
      <div className="grid max-w-4xl gap-6">
        <PanelAccessCard />
        <SecurityCard />
        <AlertSettings />
        <StandbyCard />
        <InviteCard />
        <PanelMeshCard />
      </div>
    </>
  );
}
