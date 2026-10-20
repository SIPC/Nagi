import { PageHeader } from "@/pages/layout";
import { SecurityCard } from "@/pages/security";
import { AlertSettings } from "@/pages/alerts";
import { PanelMeshCard } from "@/pages/mesh";
import { InviteCard } from "@/pages/invite";

// 面板自己的设置：账号安全、告警、面板的 mesh、接入码（让别的面板把节点交给本面板）。
export function SettingsPage() {
  return (
    <>
      <PageHeader title="设置" description="面板自身的账号安全、告警和接入设置；节点相关操作见节点页。" />
      <div className="grid max-w-4xl gap-6">
        <SecurityCard />
        <AlertSettings />
        <InviteCard />
        <PanelMeshCard />
      </div>
    </>
  );
}
