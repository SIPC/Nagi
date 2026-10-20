import { PageHeader } from "@/pages/layout";
import { SecurityCard } from "@/pages/security";
import { AlertSettings } from "@/pages/alerts";
import { PanelMeshCard } from "@/pages/mesh";
import { InviteCard } from "@/pages/invite";

// 面板自己的设置：账号安全、告警、面板的 mesh、接入码（让别的面板把节点交给本面板）。
export function SettingsPage() {
  return (
    <>
      <PageHeader title="设置" description="面板自己的设置。节点相关的操作在节点页。" />
      <SecurityCard />
      <AlertSettings />
      <InviteCard />
      <PanelMeshCard />
    </>
  );
}
