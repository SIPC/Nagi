import { ArrowUpRight } from "lucide-react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { List, Row, Section, SettingRows } from "@/components/ui";
import { useRole } from "@/components/role";
import { PanelVersion, UpdateChannelRow } from "@/pages/update";
import { settingsClient } from "@/lib/api";
import { t } from "@/i18n";

const a = t.about;
export const repo = "https://github.com/SIPC/Nagi";

// 致谢的名单和 README 一致。MiSans 的署名是字体许可的要求，不能省。
const credits = [
  { name: "Caddy", href: "https://github.com/caddyserver/caddy", use: a.credits.caddy },
  { name: "wireguard-go", href: "https://git.zx2c4.com/wireguard-go", use: a.credits.wireguard },
  { name: "Hysteria", href: "https://github.com/apernet/hysteria", use: a.credits.quic },
  { name: "quic-go", href: "https://github.com/quic-go/quic-go", use: a.credits.quic },
  { name: "kopia", href: "https://github.com/kopia/kopia", use: a.credits.kopia },
  { name: "yamux", href: "https://github.com/hashicorp/yamux", use: a.credits.control },
  { name: "Connect", href: "https://connectrpc.com", use: a.credits.control },
  { name: "shadcn/ui", href: "https://ui.shadcn.com", use: a.credits.shadcn },
  { name: "MiSans", href: "https://hyperos.mi.com/font", use: a.credits.misans },
];

// External：列表行里的外部链接。after 撑满整行（行加 relative），悬停底色的范围都能点。
function External({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a href={href} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 rounded-sm outline-none after:absolute after:inset-0 after:rounded-md hover:underline focus-ring">
      {children}
      <ArrowUpRight className="size-3.5 text-muted-foreground" />
    </a>
  );
}

// AboutCard：设置里的「关于」。所有角色都能看，只有管理员能检查和更新版本。
export function AboutCard() {
  const { isAdmin } = useRole();
  const queryClient = useQueryClient();
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}) });
  // 换渠道后面板清掉缓存的最新版本，这里重新检查（按新渠道）。
  const setChannel = async (channel: string) => {
    const r = await settingsClient.setPanelUpdateChannel({ channel });
    queryClient.setQueryData(["settings"], (s: typeof settings.data) => s && { ...s, settings: r.settings });
    await queryClient.invalidateQueries({ queryKey: ["panel-update"] });
    await queryClient.invalidateQueries({ queryKey: ["panel-release-notes"] });
  };

  return (
    <div>
      {/* 品牌和版本是第一个区块，下面的区块自带分隔线和上下 24。 */}
      <PanelVersion isAdmin={isAdmin} />
      {/* 旧面板没有更新渠道（update_channel 为空），不显示。 */}
      {settings.data?.settings?.updateChannel && (
        <Section title={a.update.title}>
          <SettingRows>
            <UpdateChannelRow value={settings.data.settings.updateChannel} help={a.update.help} canEdit={isAdmin} onChange={setChannel} />
          </SettingRows>
        </Section>
      )}
      <Section title={a.links.title}>
        <List>
          <Row className="relative" title={<External href={repo}>{a.links.source}</External>} cols={[{ phone: true, content: "GitHub" }]} />
          <Row className="relative" title={<External href={`${repo}/releases`}>{a.links.changelog}</External>} />
          <Row className="relative" title={<External href={`${repo}/issues`}>{a.links.issues}</External>} />
          <Row className="relative" title={<External href={`${repo}/blob/main/LICENSE`}>{a.links.license}</External>} cols={[{ phone: true, content: "Apache-2.0" }]} />
        </List>
      </Section>
      <Section title={a.credits.title}>
        <List>
          {credits.map((c) => (
            <Row key={c.name} className="relative" title={<External href={c.href}>{c.name}</External>} sub={c.use} />
          ))}
        </List>
      </Section>
    </div>
  );
}
