import { ArrowUpRight } from "lucide-react";
import { Link } from "@tanstack/react-router";
import { List, Row, Section, Skeleton, Status, Waves } from "@/components/ui";
import { useRole } from "@/components/role";
import { usePanelUpdate } from "@/pages/update";
import { t } from "@/i18n";

const a = t.about;
const repo = "https://github.com/SIPC/Nagi";

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

// AboutCard：设置里的「关于」。所有角色都能看，只有管理员的版本状态能点进版本更新。
export function AboutCard() {
  const { isAdmin } = useRole();
  const update = usePanelUpdate();
  const d = update.data;
  const state = d?.available ? <Status tone="info">{a.available(d.latest)}</Status> : d && !d.unsupported ? <Status tone="success">{a.upToDate}</Status> : null;

  return (
    <div>
      {/* 品牌是第一个区块：标题和区块标题同级（16 / 600），不和页面标题抢层级；下面的区块自带分隔线和上下 24。 */}
      <div className="grid justify-items-start gap-4">
        <Waves className="h-8 w-14 text-wave" />
        <div className="grid gap-1">
          <h2 className="text-base font-semibold">Nagi</h2>
          <p className="text-pretty text-muted-foreground">{a.tagline}</p>
        </div>
        <div className="flex min-h-5 flex-wrap items-center gap-x-4 gap-y-1">
          {update.isPending ? <Skeleton className="h-3 w-32" /> : <span className="font-mono text-[13px]">{d?.current || "—"}</span>}
          {state && (isAdmin ? <Link to="/settings" search={{ tab: "update" }}>{state}</Link> : state)}
        </div>
      </div>
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
