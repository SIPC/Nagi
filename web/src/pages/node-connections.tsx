// 节点页的「网络连接」标签：实时看节点上的连接、发起的进程和所属的容器（见 docs/agent.md#网络连接）。
// 节点只在这个页面开着时采样，几秒一次，不写盘。进程的命令行可能有密钥，要「节点终端和文件」，点开时再做高权限确认。
import { useEffect, useMemo, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorMessage, nodeClient } from "@/lib/api";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";
import { fmtBytes, fmtRate } from "@/components/metrics";
import { isElevationRequired, requireElevation } from "@/components/elevate";
import { useRole } from "@/components/role";
import { Badge, Button, Check, EmptyRow, Input, Notice, QueryError, RowsSkeleton, Section, Segmented, Select, Table, Td, Th } from "@/components/ui";
import type { Node } from "@/gen/nagi/v1/api_pb";
import type { ConnectionSnapshot } from "@/gen/nagi/v1/agent_pb";
import { defaultFilter, filterConns, groupConns, groupBys, hostPort, ownerOf, portHint, remoteKind, remoteLabel, type Conn, type Filter, type Group, type GroupBy } from "@/lib/connections";

const m = t.nodeConnections;

// 一次最多显示的行数：节点最多发 2000 条，全部渲染太慢；筛选后再看。
const maxRows = 300;

type State = { snap?: ConnectionSnapshot; error?: unknown; ended?: boolean };

// useConnections：打开时开始看，cmdline 变了重新连；节点到时（30 分钟）结束时 ended 为 true。
function useConnections(nodeId: string, cmdline: boolean, paused: boolean, run: number) {
  const [state, setState] = useState<State>({});
  useEffect(() => {
    if (paused) return;
    const c = new AbortController();
    setState((s) => ({ snap: s.snap }));
    (async () => {
      try {
        for await (const snap of nodeClient.watchNodeConnections({ nodeId, cmdline }, { signal: c.signal })) setState({ snap });
        if (!c.signal.aborted) setState((s) => ({ snap: s.snap, ended: true }));
      } catch (err) {
        if (!c.signal.aborted && !(err instanceof ConnectError && err.code === Code.Canceled)) setState((s) => ({ snap: s.snap, error: err }));
      }
    })();
    return () => c.abort();
  }, [nodeId, cmdline, paused, run]);
  return state;
}

export function NodeConnections({ node }: { node: Node }) {
  const { can } = useRole();
  const [cmdline, setCmdline] = useState(false);
  const [paused, setPaused] = useState(false);
  const [run, setRun] = useState(0);
  const [by, setBy] = useState<GroupBy>("process");
  const [filter, setFilter] = useState<Filter>(defaultFilter);
  const [open, setOpen] = useState<Set<string>>(new Set());
  const supported = !!node.info?.connections;
  const { snap, error, ended } = useConnections(node.id, cmdline, paused || !supported, run);

  // 命令行要求的确认过期时（或者节点那边拒绝），回到不带命令行。
  useEffect(() => {
    if (cmdline && error && isElevationRequired(error)) setCmdline(false);
  }, [cmdline, error]);

  const all: Conn[] = useMemo(() => snap?.connections ?? [], [snap]);
  const shown = useMemo(() => filterConns(all, filter), [all, filter]);
  const groups = useMemo(() => groupConns(shown, by), [shown, by]);
  const totals = useMemo(() => shown.reduce((a, c) => ({ send: a.send + c.sendRate, recv: a.recv + c.receiveRate }), { send: 0, recv: 0 }), [shown]);

  if (!supported) return <Notice title={m.oldTitle}>{m.oldText}</Notice>;

  const showCmdline = async () => {
    if (!(await requireElevation(m.elevate, { suggestTotp: false }))) return;
    setCmdline(true);
  };
  const toggle = (key: string) =>
    setOpen((s) => {
      const next = new Set(s);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  const set = (patch: Partial<Filter>) => setFilter((f) => ({ ...f, ...patch }));

  const groupName = (g: Group) => {
    switch (g.kind) {
      case "kernel":
        return m.kernel;
      case "host":
        return m.host;
      default:
        return g.label;
    }
  };

  return (
    <Section
      title={m.title}
      help={m.help}
      actions={
        <>
          {snap && <span>{m.summary(shown.length, fmtRate(totals.send), fmtRate(totals.recv), snap.intervalMs / 1000)}</span>}
          {can("node.root") && !cmdline && (
            <Button variant="outline" readOnlyOk onClick={showCmdline}>
              {m.showCmdline}
            </Button>
          )}
          <Button variant="outline" readOnlyOk onClick={() => setPaused((p) => !p)}>
            {paused ? m.resume : m.pause}
          </Button>
        </>
      }
    >
      <div className="flex flex-wrap items-center gap-2">
        <Input value={filter.query} onChange={(e) => set({ query: e.target.value })} placeholder={m.filter} aria-label={m.filter} autoComplete="off" spellCheck={false} className="max-w-[280px] max-md:max-w-none max-md:min-w-0 max-md:flex-1" />
        <Segmented value={by} onChange={setBy} label={m.groupBy} items={groupBys.map((g) => ({ value: g, label: m.group[g] }))} />
        <Select value={filter.protocol} onChange={(e) => set({ protocol: e.target.value as Filter["protocol"] })} aria-label={m.protocol} className="w-auto">
          <option value="">{m.allProtocols}</option>
          <option value="tcp">TCP</option>
          <option value="udp">UDP</option>
        </Select>
        <Check checked={filter.direction === "all"} onChange={(v) => set({ direction: v ? "all" : "out" })}>
          {m.includeInbound}
        </Check>
        <Check checked={filter.activeOnly} onChange={(v) => set({ activeOnly: v })}>
          {m.activeOnly}
        </Check>
      </div>
      {ended && (
        <Notice
          title={m.ended}
          actions={
            <Button variant="outline" readOnlyOk onClick={() => setRun((r) => r + 1)}>
              {m.resume}
            </Button>
          }
        />
      )}
      {error !== undefined && !isElevationRequired(error) ? (
        <QueryError error={errorMessage(error)} onRetry={() => setRun((r) => r + 1)} />
      ) : !snap ? (
        paused ? (
          <EmptyRow>{m.paused}</EmptyRow>
        ) : (
          <RowsSkeleton count={4} />
        )
      ) : (
        <>
          {(snap.omitted > 0 || snap.namespaceErrors > 0 || snap.intervalMs > 2000) && (
            <p className="text-[13px] text-muted-foreground">
              {[snap.omitted > 0 && m.omitted(snap.omitted), snap.intervalMs > 2000 && m.slowed(snap.tookMs), snap.namespaceErrors > 0 && m.nsErrors(snap.namespaceErrors)].filter(Boolean).join(m.join)}
            </p>
          )}
          {shown.length === 0 ? (
            <EmptyRow>{all.length === 0 ? m.empty : m.noMatch}</EmptyRow>
          ) : by === "none" ? (
            <ConnTable conns={shown} cmdline={cmdline} />
          ) : (
            <Table compact className="table-fixed">
              <thead>
                <tr>
                  <Th>{m.group[by]}</Th>
                  <Th className="w-24 text-right max-md:w-12">{m.connections}</Th>
                  <Th className="w-28 text-right max-md:w-20">{m.send}</Th>
                  <Th className="w-28 text-right max-md:w-20">{m.receive}</Th>
                </tr>
              </thead>
              <tbody>
                {groups.slice(0, maxRows).map((g) => {
                  const isOpen = open.has(g.key);
                  return [
                    <tr key={g.key} className="cursor-pointer" onClick={() => toggle(g.key)} aria-expanded={isOpen}>
                      <Td className="truncate">
                        <span className={cn("mr-1 inline-block w-3 text-muted-foreground transition-transform", isOpen && "rotate-90")}>›</span>
                        <span className={cn(g.kind === "kernel" || g.kind === "host" ? "text-muted-foreground" : "font-mono text-[13px]")}>{groupName(g)}</span>
                        {by === "remote" && <RemoteTag conn={g.conns[0]} />}
                      </Td>
                      <Td className="text-right tabular-nums">{g.conns.length}</Td>
                      <Td className="text-right tabular-nums">{fmtRate(g.sendRate)}</Td>
                      <Td className="text-right tabular-nums">{fmtRate(g.receiveRate)}</Td>
                    </tr>,
                    isOpen && (
                      <tr key={g.key + ":detail"} className="[&:hover>td]:bg-transparent!">
                        <Td colSpan={4} className="pb-3">
                          <ConnTable conns={g.conns} cmdline={cmdline} hide={by} />
                        </Td>
                      </tr>
                    ),
                  ];
                })}
              </tbody>
            </Table>
          )}
          {groups.length > maxRows && <p className="text-[13px] text-muted-foreground">{m.moreGroups(groups.length - maxRows)}</p>}
        </>
      )}
    </Section>
  );
}

// ConnTable：连接的明细。hide 是已经按它分组的那一列，不再重复显示。
function ConnTable({ conns, cmdline, hide }: { conns: Conn[]; cmdline: boolean; hide?: GroupBy }) {
  const rows = conns.slice(0, maxRows);
  return (
    <>
      <Table compact className="table-fixed">
        <thead>
          <tr>
            <Th className="w-14">{m.protocol}</Th>
            {hide !== "remote" && <Th>{m.remote}</Th>}
            <Th className="w-28 max-md:hidden">{m.state}</Th>
            {/* 按进程分组时名称在组上，这一列只写 PID 和命令行（同名的几个进程分得开）。 */}
            <Th className="max-md:hidden">{hide === "process" ? m.pid : m.process}</Th>
            {hide !== "container" && <Th className="max-md:hidden">{m.owner}</Th>}
            <Th className="w-24 text-right">{m.send}</Th>
            <Th className="w-24 text-right">{m.receive}</Th>
            <Th className="w-24 text-right max-lg:hidden">{m.total}</Th>
          </tr>
        </thead>
        <tbody>
          {rows.map((c, i) => {
            const o = ownerOf(c);
            return (
              <tr key={`${c.localIp}:${c.localPort}-${c.remoteIp}:${c.remotePort}-${c.protocol}-${i}`} className="align-top">
                <Td className="text-muted-foreground uppercase">{c.protocol}</Td>
                {hide !== "remote" && (
                  <Td className="truncate">
                    <span className="font-mono text-[13px]" title={`${c.localIp ? hostPort(c.localIp, c.localPort) : c.localPort} → ${remoteLabel(c)}`}>
                      {c.inbound && <span className="text-muted-foreground">{m.inbound} </span>}
                      {remoteLabel(c)}
                    </span>
                    <RemoteTag conn={c} />
                  </Td>
                )}
                <Td className="text-muted-foreground max-md:hidden">{c.state || "—"}</Td>
                <Td className="truncate max-md:hidden" title={c.cmdline || undefined}>
                  {c.pid ? (
                    <>
                      {hide !== "process" && `${c.process} `}
                      <span className="text-muted-foreground tabular-nums">{c.pid}</span>
                    </>
                  ) : (
                    <span className="text-muted-foreground">{hide === "process" ? "—" : m.kernel}</span>
                  )}
                  {cmdline && c.cmdline && <span className="block truncate font-mono text-xs text-muted-foreground">{c.cmdline}</span>}
                </Td>
                {hide !== "container" && <Td className="truncate text-muted-foreground max-md:hidden">{o.kind === "container" || o.kind === "unit" ? o.label : "—"}</Td>}
                <Td className="text-right tabular-nums">{c.sendRate > 0 ? fmtRate(c.sendRate) : "—"}</Td>
                <Td className="text-right tabular-nums">{c.receiveRate > 0 ? fmtRate(c.receiveRate) : "—"}</Td>
                <Td className="text-right text-muted-foreground tabular-nums max-lg:hidden">{c.bytesSent + c.bytesReceived > 0n ? fmtBytes(Number(c.bytesSent + c.bytesReceived)) : "—"}</Td>
              </tr>
            );
          })}
        </tbody>
      </Table>
      {conns.length > maxRows && <p className="mt-2 text-[13px] text-muted-foreground">{m.moreRows(conns.length - maxRows)}</p>}
    </>
  );
}

// RemoteTag：远端后面的小标签：Nagi 节点名、本机、内网、节点网络，再加常见端口的提示（例如 iSCSI）。
function RemoteTag({ conn }: { conn: Conn }) {
  const kind = remoteKind(conn.remoteIp);
  const hint = portHint(conn);
  // 远端是面板时节点名就是「面板」，不加「节点」；没给地址时名称已经在地址的位置上。
  const label = !conn.remoteIp ? "" : conn.remoteNodeName ? (conn.remoteNodeName === m.panel ? m.panel : m.node(conn.remoteNodeName)) : kind === "public" ? "" : m.kinds[kind];
  if (!label && !hint) return null;
  return (
    <span className="ml-2 inline-flex gap-1 align-middle">
      {label && <Badge>{label}</Badge>}
      {hint && <Badge tone="info">{hint}</Badge>}
    </span>
  );
}
