import { Fragment, useContext, useEffect, useState } from "react";
import type * as React from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowUpRight, Plus } from "lucide-react";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";
import { Alert, Button, Check, CopyButton, Dialog, DialogActions, EmptyRow, Field, FormRow, Input, List, QueryError, ReadOnlyContext, Row, RowMenu, RowsSkeleton, Section, Select, Skeleton, Spinner, Status, dotColor } from "@/components/ui";
import type { DomainCheck } from "@/gen/nagi/v1/api_pb";
import type { Container, Exposure, ExposureSchema, Project, ServiceStatus } from "@/gen/nagi/v1/common_pb";
import { confirmAction } from "@/components/confirm";
import { toast } from "sonner";

// 「网络」标签页：对外访问（端口映射与域名）、项目内网、项目连接（见 docs/agent.md#项目连接）。

export function NetworkTab({ project, services }: { project: Project; services: ServiceStatus[] }) {
  return (
    <div>
      <RoutesOverview project={project} />
      <Intranet project={project} services={services} />
      <Links project={project} services={services} />
    </div>
  );
}

// ContainerNetwork：容器页的「网络」标签：这个容器的端口映射，以及别的容器怎么访问它。
export function ContainerNetwork({ project, container, services }: { project: Project; container: Container; services: ServiceStatus[] }) {
  return (
    <div>
      <RoutesSection project={project} services={services} container={container.name} />
      <InternalAccess project={project} container={container} services={services} />
    </div>
  );
}

// HelpParts：问号里的几段说明，一段一行。
function HelpParts({ parts }: { parts: React.ReactNode[] }) {
  return (
    <span className="grid gap-1">
      {parts.map((p, i) => (
        <span key={i}>{p}</span>
      ))}
    </span>
  );
}

// InternalAccess：项目内的容器访问这个容器用的地址，连接的项目再加上网络名。
function InternalAccess({ project, container, services }: { project: Project; container: Container; services: ServiceStatus[] }) {
  const m = t.projectNetwork.access;
  const own = services.filter((s) => s.container === container.name);
  const names = intranetAddresses(project, services)
    .filter((a) => (container.kind === "group" ? a.name.endsWith(`.${container.name}`) : a.name === container.name))
    .map((a) => a.name);
  const ports = (name: string) => {
    const service = container.kind === "group" ? name.slice(0, name.lastIndexOf(".")) : name;
    const list = own.filter((s) => container.kind !== "group" || s.name === service);
    const declared = [...list.flatMap((s) => s.ports.map(String)), ...list.flatMap((s) => s.udpPorts.map((p) => `${p}/udp`))];
    const internal = (project.settings?.internalPorts ?? []).flatMap((p) => (p.startsWith(`${name}:`) ? [p.slice(name.length + 1)] : []));
    return [...new Set([...declared, ...internal])];
  };
  const addresses = names.flatMap((name) => {
    const list = ports(name);
    return list.length > 0 ? list.map((p) => ({ address: `${name}:${p.replace(/\/udp$/, "")}`, udp: p.endsWith("/udp") })) : [{ address: name, udp: false }];
  });
  return (
    <Section title={m.title} help={m.help(project.networkName)}>
      {addresses.length === 0 ? (
        <EmptyRow>{t.projectNetwork.intranet.empty}</EmptyRow>
      ) : (
        <List>
          {addresses.map((a) => (
            <Row
              key={a.address + a.udp}
              actionsWidth="w-auto"
              title={
                <>
                  <span className="min-w-0 wrap-anywhere">{a.address}</span>
                  {a.udp && <span className="text-muted-foreground">UDP</span>}
                </>
              }
              actions={<CopyButton text={a.address} className="-my-1.5 -mr-2 max-md:-my-2.5 max-md:-mr-3 max-md:size-10" />}
            />
          ))}
        </List>
      )}
    </Section>
  );
}

// ---- 对外访问：共用的部分 ----

type Route = Project["routes"][number];
type RouteState = keyof typeof t.projectNetwork.routes.status;

const stateTone: Record<RouteState, keyof typeof dotColor> = {
  ok: "success",
  mismatch: "warning",
  pending: "warning",
  dnsFailed: "danger",
  certFailed: "danger",
  inactive: "danger",
  unknown: "neutral",
  checking: "neutral",
};

// invalidReason：映射没有生效的原因，生效时为空。
function invalidReason(r: Route) {
  if (r.noLocalEntry && r.entryNodes.length === 0) return t.projectNetwork.routes.noEntries;
  return r.error;
}

// routeState 把一条映射的各入口节点上的解析和证书情况合成一个状态，只取最需要处理的那个。
function routeState(r: Route, checks: DomainCheck[], checking: boolean): RouteState {
  if (invalidReason(r)) return "inactive";
  if (r.domains.length === 0) return "ok";
  const entries = checks.flatMap((c) => c.entries);
  if (entries.length === 0) return checking ? "checking" : "unknown";
  if (entries.some((e) => e.cert === "failed")) return "certFailed";
  if (entries.some((e) => e.dns === "unresolved")) return "dnsFailed";
  if (entries.some((e) => e.dns === "mismatch")) return "mismatch";
  if (entries.some((e) => e.cert === "pending")) return "pending";
  if (entries.some((e) => e.dns === "unknown" || e.cert === "unknown")) return "unknown";
  return "ok";
}

function RouteStatus({ state }: { state: RouteState }) {
  const label = t.projectNetwork.routes.status[state];
  const tone = stateTone[state];
  return <Status tone={tone}>{tone === "danger" ? <span className="text-destructive">{label}</span> : tone === "warning" ? <span className="text-warning-text">{label}</span> : label}</Status>;
}

// routeProblems 是第三行的说明：每个域名在每个入口节点上有问题的地方。只有一个入口时不用写节点名。
function routeProblems(checks: DomainCheck[]) {
  const m = t.projectNetwork.routes.check;
  const manyDomains = checks.length > 1;
  const manyEntries = checks.flatMap((c) => c.entries).length > 1;
  return checks.flatMap((c) =>
    c.entries.flatMap((e) => {
      const prefix = `${manyDomains ? `${c.domain} ` : ""}${manyEntries ? `${e.nodeName} ` : ""}`;
      const list: { text: string; danger?: boolean; cert?: boolean }[] = [];
      if (e.dns === "mismatch") list.push({ text: m.dnsMismatch(c.addresses.join(", ")) });
      if (e.dns === "unresolved") list.push({ text: m.dnsUnresolved, danger: true });
      if (e.dns === "unknown") list.push({ text: m.dnsUnknown });
      if (e.cert === "failed") list.push({ text: m.certFailed, danger: true, cert: true });
      if (e.cert === "unknown") list.push({ text: m.certUnknown });
      if (list.length === 0) return [];
      list[0].text = prefix + list[0].text;
      return [{ key: `${c.domain}/${e.nodeId}`, parts: list }];
    }),
  );
}

// certSummary：第二行最后一段的证书情况。多个入口时写最早到期的那张。
function certSummary(checks: DomainCheck[]) {
  const m = t.projectNetwork.routes;
  const entries = checks.flatMap((c) => c.entries);
  if (entries.some((e) => e.cert === "pending")) return entries.every((e) => e.dns === "ok") ? `${m.check.dnsOk} · ${m.check.certPending}` : m.check.certPending;
  if (entries.length === 0 || entries.some((e) => e.cert !== "obtained")) return "";
  const dates = entries.flatMap((e) => (e.certNotAfter ? [timestampDate(e.certNotAfter)] : []));
  if (dates.length === 0) return m.check.certObtained();
  const earliest = new Date(Math.min(...dates.map((d) => d.getTime())));
  const pad = (n: number) => String(n).padStart(2, "0");
  return m.certExpires(`${earliest.getFullYear()}-${pad(earliest.getMonth() + 1)}-${pad(earliest.getDate())}`);
}

function useRouteNodes(project: Project) {
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;
  // 映射开在它的入口节点上（默认是容器所在的节点），访问地址用这些节点的地址。
  const entriesOf = (r: Route) => {
    const own = project.containers.find((c) => c.name === r.container)?.nodeId ?? "";
    return [...new Set([...(r.noLocalEntry ? [] : [own]), ...r.entryNodes])].filter(Boolean);
  };
  const addressesOf = (r: Route) =>
    entriesOf(r)
      .map((id) => nodes.data?.nodes.find((n) => n.id === id)?.address ?? "")
      .filter(Boolean);
  return { nodes, nodeName, entriesOf, addressesOf };
}

// RouteTitle：第一行的访问地址。http 绑了域名用域名，否则用"节点地址:端口"；tcp/udp 只给地址，不是网页。
// 映射没生效或者不知道地址时写映射名。
function RouteTitle({ route: r, addresses, linkClassName }: { route: Route; addresses: string[]; linkClassName?: string }) {
  const domains = r.domains.map((d) => `${r.httpOnly ? "http" : "https"}://${d}`);
  const list = r.published ? addresses : [];
  if (invalidReason(r) || (domains.length === 0 && list.length === 0)) return <span className="min-w-0 wrap-anywhere">{r.name}</span>;
  return (
    <span className="flex min-w-0 flex-wrap gap-x-4">
      {domains.map((url) => (
        <RouteLink key={url} url={url} className={linkClassName} />
      ))}
      {domains.length === 0 &&
        list.map((address) =>
          r.protocol === "http" ? (
            <RouteLink key={address} url={`http://${address}:${r.published}`} className={linkClassName} />
          ) : (
            <span key={address} className="min-w-0 wrap-anywhere">
              {`${address}:${r.published}`}
            </span>
          ),
        )}
    </span>
  );
}

// RouteSub：第二行「名称 → 目标 · 入口 · 证书或协议」，有问题时加第三行。
// overview：项目网络的总览，不写证书（状态在右边），节点端口的映射也写入口；容器页上地址已经带了入口节点，节点端口的映射不再写。
// onReason：证书失败那一行后面的「查看原因」。
function RouteSub({ route: r, checks, entries, hasAddress, overview, onReason }: { route: Route; checks: DomainCheck[]; entries: string[]; hasAddress: boolean; overview?: boolean; onReason?: () => void }) {
  const m = t.projectNetwork.routes;
  const target = `${r.container}${r.service !== r.container ? `/${r.service}` : ""}:${r.target}`;
  const reason = invalidReason(r);
  if (reason) {
    return (
      <>
        {`${target} · `}
        <span className="text-destructive">{reason}</span>
      </>
    );
  }
  const protocol = r.protocol.toUpperCase();
  const last = r.domains.length > 0 ? (r.httpOnly ? m.httpOnlyBadge : certSummary(checks)) : hasAddress ? protocol : `${protocol}/${r.published}`;
  const entry = entries.length > 0 ? m.entryList([...entries].sort().join(", ")) : "";
  const parts = r.domains.length > 0 ? [entry, overview ? "" : last] : overview ? [last, entry] : hasAddress ? [last] : [entry, last];
  const line = [`${r.name} → ${target}`, ...parts].filter(Boolean);
  return (
    <>
      <div>{line.join(" · ")}</div>
      {routeProblems(checks).map((p) => (
        <div key={p.key}>
          {p.parts.map((part, i) => (
            <Fragment key={i}>
              {i > 0 && " · "}
              {part.danger ? <span className="text-destructive">{part.text}</span> : part.text}
              {part.cert && onReason && (
                <>
                  {" · "}
                  <button type="button" className="cursor-pointer rounded-sm text-foreground underline decoration-border underline-offset-4 outline-none focus-visible:ring-2 focus-visible:ring-ring/40" onClick={onReason}>
                    {m.check.certErrorDetail}
                  </button>
                </>
              )}
            </Fragment>
          ))}
        </div>
      ))}
    </>
  );
}

function RouteLink({ url, className }: { url: string; className?: string }) {
  return (
    <a href={url} target="_blank" rel="noreferrer" className={cn("inline-flex min-w-0 items-center gap-1 rounded-sm wrap-anywhere outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring/40", className)}>
      {url.replace(/^https?:\/\//, "")}
      <ArrowUpRight strokeWidth={2.25} className="size-3 shrink-0 text-muted-foreground" />
    </a>
  );
}

// useDomainChecks 检查映射绑定的域名：是否解析到入口节点、证书状态。没有要查的映射时返回空。
function useDomainChecks(projectId: string, routes: Exposure[] | MessageInitShape<typeof ExposureSchema>[], refetchInterval?: number) {
  const key = routes.map((r) => [r.name, r.container, r.domains, r.entryNodes, r.noLocalEntry, r.httpOnly]);
  const q = useQuery({
    queryKey: ["domain-checks", projectId, key],
    queryFn: () => projectClient.checkDomains({ projectId, routes }),
    enabled: routes.length > 0,
    refetchInterval,
    placeholderData: (prev) => prev,
  });
  const on = routes.length > 0;
  return { checks: on ? (q.data?.domains ?? []) : [], pending: on && q.isPending, error: on ? q.error : null, refetch: q.refetch };
}

// RoutesOverview：项目页上全部端口映射的总览。修改在容器页，整行链过去。
function RoutesOverview({ project }: { project: Project }) {
  const m = t.projectNetwork.routes;
  const { nodes, nodeName, entriesOf, addressesOf } = useRouteNodes(project);
  const domains = useDomainChecks(project.id, project.routes.filter((r) => r.domains.length > 0), 15_000);
  return (
    <Section title={m.exposedTitle} help={m.overviewDescription}>
      {nodes.isError && <QueryError error={nodes.error} onRetry={() => nodes.refetch()} />}
      {domains.error && <QueryError error={domains.error} onRetry={() => domains.refetch()} />}
      {project.routes.length === 0 ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : nodes.isPending ? (
        <RowsSkeleton count={Math.min(project.routes.length, 6)} />
      ) : (
        <List>
          {project.routes.map((r) => {
            const checks = domains.checks.filter((c) => c.route === r.name);
            const addresses = addressesOf(r);
            return (
              <div key={r.name} className="group/route relative">
                {/* 盖满整行的真链接：能 Ctrl+点击、能用键盘，访问地址抬到它上面，不用把链接套在链接里。 */}
                <Link
                  to="/projects/$projectId/c/$container"
                  params={{ projectId: project.id, container: r.container }}
                  search={{ view: "network" }}
                  aria-label={r.name}
                  className="absolute inset-0 rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
                />
                <Row
                  className="group-hover/route:bg-accent"
                  title={<RouteTitle route={r} addresses={addresses} linkClassName="relative z-10" />}
                  sub={<RouteSub overview route={r} checks={checks} entries={entriesOf(r).map(nodeName)} hasAddress={addresses.length > 0} />}
                  cols={[{ content: <RouteStatus state={routeState(r, checks, domains.pending)} />, phone: true }]}
                />
              </div>
            );
          })}
        </List>
      )}
    </Section>
  );
}

// ---- 端口映射 ----

function RoutesSection({ project, services, container }: { project: Project; services: ServiceStatus[]; container: string }) {
  const queryClient = useQueryClient();
  const m = t.projectNetwork.routes;
  const readOnly = useContext(ReadOnlyContext);
  const routes = project.routes.filter((r) => r.container === container);
  const empty = {
    name: "",
    service: "",
    target: "",
    protocol: "http",
    published: "",
    domains: "",
    httpOnly: false,
    localEntry: true,
    entryNodes: [] as string[],
  };
  const [form, setForm] = useState(empty);
  const [open, setOpen] = useState(false);
  const [formError, setFormError] = useState("");
  const [listError, setListError] = useState("");
  const [busy, setBusy] = useState(false);
  const [retrying, setRetrying] = useState("");
  const [reasonOf, setReasonOf] = useState("");
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["project", project.id] });
  const containerServices = [...new Set(services.filter((s) => s.container === container).map((s) => s.name))];
  const isGroup = project.containers.find((c) => c.name === container)?.kind === "group";
  const { nodes, nodeName, entriesOf, addressesOf } = useRouteNodes(project);
  const containerNode = project.containers.find((c) => c.name === container)?.nodeId ?? "";
  const toggleEntry = (id: string, on: boolean) =>
    setForm({ ...form, entryNodes: on ? [...form.entryNodes, id] : form.entryNodes.filter((n) => n !== id) });
  // 只有 http 能绑域名；绑了域名经 80/443 访问，不用节点端口。
  const isHTTP = form.protocol === "http";
  const domainList = isHTTP ? form.domains.split(/[\s,]+/).filter(Boolean) : [];
  const hasDomains = domainList.length > 0;
  const multipleEntries = (form.localEntry ? 1 : 0) + form.entryNodes.length > 1;
  const draftRoute = {
    name: form.name,
    container,
    service: form.service || containerServices[0] || "",
    target: Number(form.target),
    protocol: form.protocol,
    published: hasDomains ? 0 : Number(form.published || 0),
    domains: domainList,
    httpOnly: hasDomains && form.httpOnly,
    entryNodes: form.entryNodes,
    noLocalEntry: !form.localEntry,
  };
  // 已保存的映射定时检查域名的解析和证书；编辑时停下来再查，只用解析结果。
  const savedDomains = useDomainChecks(project.id, routes.filter((r) => r.domains.length > 0), 15_000);
  const draftKey = JSON.stringify([domainList, container, form.entryNodes, form.localEntry]);
  const [settledKey, setSettledKey] = useState(draftKey);
  useEffect(() => {
    const timer = setTimeout(() => setSettledKey(draftKey), 600);
    return () => clearTimeout(timer);
  }, [draftKey]);
  const draftDomains = useDomainChecks(project.id, open && hasDomains && settledKey === draftKey ? [draftRoute] : []);

  const openAdd = () => {
    setForm(empty);
    setFormError("");
    setOpen(true);
  };

  const openEdit = (r: Route) => {
    setForm({
      name: r.name,
      service: r.service,
      target: String(r.target),
      protocol: r.protocol || "http",
      published: r.published ? String(r.published) : "",
      domains: r.domains.join(", "),
      httpOnly: r.httpOnly,
      localEntry: !r.noLocalEntry,
      entryNodes: r.entryNodes,
    });
    setFormError("");
    setOpen(true);
  };

  const save = async () => {
    setFormError("");
    setBusy(true);
    try {
      await projectClient.saveRoute({
        projectId: project.id,
        route: draftRoute,
      });
      setForm(empty);
      setOpen(false);
      refresh();
    } catch (err) {
      setFormError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (name: string) => {
    if (!(await confirmAction(m.deleteConfirm(name), { action: t.common.actions.delete, danger: true }))) return;
    setListError("");
    try {
      await projectClient.deleteRoute({ projectId: project.id, name });
      refresh();
    } catch (err) {
      setListError(errorMessage(err));
    }
  };

  // 重试这条映射上所有申请失败的证书。
  const retry = async (route: string, checks: DomainCheck[]) => {
    setRetrying(route);
    setListError("");
    try {
      await Promise.all(
        checks.flatMap((c) => c.entries.filter((e) => e.cert === "failed").map((e) => projectClient.retryDomainCert({ projectId: project.id, route: c.route, domain: c.domain, nodeId: e.nodeId }))),
      );
      await queryClient.invalidateQueries({ queryKey: ["domain-checks", project.id] });
    } catch (err) {
      setListError(errorMessage(err));
    } finally {
      setRetrying("");
    }
  };

  const certErrors = (checks: DomainCheck[]) => checks.flatMap((c) => c.entries.filter((e) => e.certError).map((e) => ({ domain: c.domain, node: e.nodeName, nodeId: e.nodeId, error: e.certError })));
  const shownReasons = certErrors(savedDomains.checks.filter((c) => c.route === reasonOf));

  return (
    <Section
      title={m.exposedTitle}
      help={m.help}
      actions={
        routes.length > 0 && (
          <Button variant="outline" onClick={openAdd}>
            <Plus />
            {m.add}
          </Button>
        )
      }
    >
      {listError && <Alert>{listError}</Alert>}
      {nodes.isError && <QueryError error={nodes.error} onRetry={() => nodes.refetch()} />}
      {savedDomains.error && <QueryError error={savedDomains.error} onRetry={() => savedDomains.refetch()} />}
      {routes.length === 0 ? (
        <div className="grid justify-items-center gap-6 py-8 text-center">
          <p className="text-muted-foreground">{m.emptyContainer}</p>
          <Button onClick={openAdd}>
            <Plus />
            {m.add}
          </Button>
        </div>
      ) : nodes.isPending ? (
        <RowsSkeleton count={Math.min(routes.length, 6)} />
      ) : (
        <List>
          {routes.map((r) => {
            const checks = savedDomains.checks.filter((c) => c.route === r.name);
            const addresses = addressesOf(r);
            const failed = checks.some((c) => c.entries.some((e) => e.cert === "failed"));
            const reasons = certErrors(checks);
            const invalid = !!invalidReason(r);
            return (
              <Row
                key={r.name}
                actionsWidth="w-28"
                title={<RouteTitle route={r} addresses={addresses} />}
                sub={<RouteSub route={r} checks={checks} entries={entriesOf(r).map(nodeName)} hasAddress={addresses.length > 0} onReason={reasons.length > 0 ? () => setReasonOf(r.name) : undefined} />}
                cols={[{ content: <RouteStatus state={routeState(r, checks, savedDomains.pending)} />, phone: true }]}
                actions={
                  <>
                    {invalid ? (
                      <Button variant="ghost" size="row" onClick={() => openEdit(r)}>
                        {t.common.actions.edit}
                      </Button>
                    ) : readOnly && reasons.length > 0 ? (
                      // 只读时 ⋮ 整个置灰，查看原因不改东西，单独放出来。
                      <Button variant="ghost" size="row" readOnlyOk onClick={() => setReasonOf(r.name)}>
                        {m.check.certErrorDetail}
                      </Button>
                    ) : failed ? (
                      <Button variant="ghost" size="row" disabled={retrying === r.name} onClick={() => retry(r.name, checks)}>
                        {retrying === r.name ? m.check.certRetrying : t.common.actions.retry}
                      </Button>
                    ) : null}
                    <RowMenu
                      items={[
                        ...(reasons.length > 0 ? [{ label: m.check.certErrorDetail, onSelect: () => setReasonOf(r.name) }] : []),
                        { label: t.common.actions.edit, onSelect: () => openEdit(r) },
                        { label: t.common.actions.delete, danger: true, onSelect: () => remove(r.name) },
                      ]}
                    />
                  </>
                }
              />
            );
          })}
        </List>
      )}

      {/* 重试成功后原因没了，对话框随之关闭。 */}
      <Dialog open={shownReasons.length > 0} onClose={() => setReasonOf("")} title={m.check.certErrorDetail} className="max-w-[520px]">
        {shownReasons.map((c) => (
          <div key={`${c.domain}/${c.nodeId}`} className="grid gap-1">
            <div className="wrap-anywhere">
              {c.domain} · {c.node}
            </div>
            <div className="text-[13px] leading-5 wrap-anywhere text-destructive">{c.error}</div>
          </div>
        ))}
      </Dialog>

      <Dialog open={open} onClose={() => setOpen(false)} title={form.name ? m.edit(form.name) : m.add} className="max-w-[520px]">
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            save();
          }}
        >
          {isGroup && (
            <Field label={m.groupService} htmlFor="route-service">
              <Select id="route-service" value={form.service || containerServices[0] || ""} onChange={(e) => setForm({ ...form, service: e.target.value })}>
                {containerServices.map((s) => (
                  <option key={s}>{s}</option>
                ))}
              </Select>
            </Field>
          )}
          <FormRow className="max-md:grid-cols-2">
            <Field label={m.containerPort} htmlFor="route-target">
              <Input id="route-target" required inputMode="numeric" placeholder="80" value={form.target} onChange={(e) => setForm({ ...form, target: e.target.value })} />
            </Field>
            <Field label={m.protocol} htmlFor="route-protocol">
              <Select id="route-protocol" value={form.protocol} onChange={(e) => setForm({ ...form, protocol: e.target.value })}>
                <option value="http">HTTP</option>
                <option value="tcp">TCP</option>
                <option value="udp">UDP</option>
              </Select>
            </Field>
          </FormRow>
          {isHTTP && (
            <Field
              label={m.domains}
              htmlFor="route-domains"
              help={m.domainsHelp}
              hint={hasDomains ? <DraftDns checks={draftDomains.checks} error={draftDomains.error} /> : undefined}
            >
              <Input id="route-domains" placeholder="app.example.com" value={form.domains} onChange={(e) => setForm({ ...form, domains: e.target.value })} />
            </Field>
          )}
          {!hasDomains && (
            <Field label={m.nodePort} htmlFor="route-published" help={m.nodePortHelp}>
              <Input id="route-published" required inputMode="numeric" placeholder="8080" value={form.published} onChange={(e) => setForm({ ...form, published: e.target.value })} />
            </Field>
          )}
          <Field label={m.entryNodes} help={m.entryNodesHelp}>
            {nodes.isPending ? (
              <Skeleton className="h-5 w-64 max-w-full" />
            ) : (
              <div className="flex flex-wrap gap-x-6 gap-y-2 [&_label]:leading-[14px] [&_label]:font-medium">
                <Check checked={form.localEntry} onChange={(v) => setForm({ ...form, localEntry: v })}>
                  {m.localEntry(nodeName(containerNode))}
                </Check>
                {(nodes.data?.nodes ?? [])
                  .filter((n) => n.id !== containerNode || form.entryNodes.includes(n.id))
                  .map((n) => (
                    <Check key={n.id} checked={form.entryNodes.includes(n.id)} onChange={(v) => toggleEntry(n.id, v)}>
                      {n.name}
                    </Check>
                  ))}
              </div>
            )}
            {nodes.isError && <QueryError error={nodes.error} onRetry={() => nodes.refetch()} />}
            {hasDomains && multipleEntries && !form.httpOnly && <p className="leading-5 text-warning-text">{m.entryDomainWarningShort}</p>}
            {!hasDomains && form.entryNodes.length > 0 && <p className="leading-5 text-muted-foreground">{m.entryCrossHint}</p>}
          </Field>
          {hasDomains && (
            <Check checked={form.httpOnly} onChange={(v) => setForm({ ...form, httpOnly: v })} help={m.httpOnlyHelp}>
              {m.httpOnly}
            </Check>
          )}
          {formError && <Alert>{formError}</Alert>}
          <DialogActions>
            <Button variant="outline" onClick={() => setOpen(false)}>
              {t.common.actions.cancel}
            </Button>
            <Button type="submit" disabled={busy}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          </DialogActions>
        </form>
      </Dialog>
    </Section>
  );
}

// DraftDns：映射还没保存，证书无从谈起，域名下面只写各入口节点的解析结果。
function DraftDns({ checks, error }: { checks: DomainCheck[]; error: unknown }) {
  const m = t.projectNetwork.routes.check;
  if (error) return <span className="text-destructive">{errorMessage(error)}</span>;
  const multiple = checks.length > 1;
  return (
    <>
      {checks.flatMap((c) =>
        c.entries.map((e) => (
          <span key={`${c.domain}/${e.nodeId}`} className={cn("block text-sm", e.dns === "mismatch" && "text-warning-text", e.dns === "unresolved" && "text-destructive")}>
            {`${multiple ? `${c.domain} ` : ""}${e.nodeName}: ${e.dns === "mismatch" ? m.dnsMismatch(c.addresses.join(", ")) : e.dns === "ok" ? m.dnsOk : e.dns === "unresolved" ? m.dnsUnresolved : m.dnsUnknown}`}
          </span>
        )),
      )}
    </>
  );
}

// ---- 项目内网 ----

// intranetAddresses 列出项目里每个容器在项目内网里的名字：单个容器是"容器名"，容器组里的是"容器.容器组"。
// 容器组还没有运行、不知道里面有哪些容器时，是一个占位的名字（placeholder）。
function intranetAddresses(project: Project, services: ServiceStatus[]) {
  return project.containers.flatMap((c) => {
    if (c.kind !== "group") return [{ name: c.name, node: c.nodeId, placeholder: false }];
    const names = [...new Set(services.filter((s) => s.container === c.name).map((s) => s.name))];
    if (names.length === 0) return [{ name: `${t.projectNetwork.intranet.servicePlaceholder}.${c.name}`, node: c.nodeId, placeholder: true }];
    return names.map((s) => ({ name: `${s}.${c.name}`, node: c.nodeId, placeholder: false }));
  });
}

const splitPorts = (text: string) => text.split(/[\s,]+/).filter(Boolean);

// Intranet 显示项目内网里的名字和它们的内部端口（只改这一项，其余设置照原样提交）。
// 接口存的是"名字:端口"的列表，界面上按名字分行；对不上任何一行的（容器已删除、容器组还没运行）放在「其他」，原样保留。
function Intranet({ project, services }: { project: Project; services: ServiceStatus[] }) {
  const m = t.projectNetwork.intranet;
  const queryClient = useQueryClient();
  const readOnly = useContext(ReadOnlyContext);
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;
  // 只记改过的行，没改的取项目里的值，服务列表后到时也分得对。
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [othersEdit, setOthersEdit] = useState<string>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const addresses = intranetAddresses(project, services);
  const known = new Set(addresses.filter((a) => !a.placeholder).map((a) => a.name));
  const saved: Record<string, string[]> = {};
  const savedOthers: string[] = [];
  for (const p of project.settings?.internalPorts ?? []) {
    const i = p.indexOf(":");
    const name = i < 0 ? "" : p.slice(0, i);
    if (known.has(name)) (saved[name] ??= []).push(p.slice(i + 1));
    else savedOthers.push(p);
  }
  const valueOf = (name: string) => edits[name] ?? (saved[name] ?? []).join(" ");
  const others = othersEdit ?? savedOthers.join(" ");
  const showOthers = savedOthers.length > 0 || others !== "" || addresses.some((a) => a.placeholder);

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await projectClient.updateProjectSettings({
        projectId: project.id,
        baseRevision: project.revision,
        name: project.name,
        networkName: project.networkName,
        settings: {
          snapshotIntervalMinutes: project.settings?.snapshotIntervalMinutes ?? 0,
          internalPorts: [...[...known].flatMap((name) => splitPorts(valueOf(name)).map((p) => `${name}:${p}`)), ...splitPorts(others)],
        },
      });
      toast.success(t.common.feedback.saved);
      await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      setEdits({});
      setOthersEdit(undefined);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const hasRows = addresses.length > 0 || showOthers;
  return (
    <Section
      title={m.title}
      help={<HelpParts parts={[m.description, m.help, m.portsRowHelp, m.internalPortsHelp]} />}
      actions={
        <>
          <span>{`${t.projectNetwork.links.columns.networkName} ${project.networkName}`}</span>
          {hasRows && (
            <Button variant="outline" onClick={save} disabled={busy}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          )}
        </>
      }
    >
      {error && <Alert>{error}</Alert>}
      {nodes.isError && <QueryError error={nodes.error} onRetry={() => nodes.refetch()} />}
      {!hasRows ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : (
        <List>
          {addresses.map((a, i) => (
            <Row
              key={a.name}
              className="min-h-14 py-2"
              title={<span className="min-w-0 wrap-anywhere">{a.name}</span>}
              sub={i === 0 && !a.placeholder ? m.portsRowSub(a.name) : undefined}
              cols={[{ content: nodes.isPending ? <Skeleton className="h-3 w-12" /> : <Status>{nodeName(a.node)}</Status>, phone: true }]}
              actions={
                a.placeholder ? null : (
                  <Input
                    aria-label={`${m.internalPorts} ${a.name}`}
                    className="w-[120px] shrink-0"
                    placeholder="80 9100"
                    disabled={readOnly}
                    value={valueOf(a.name)}
                    onChange={(e) => setEdits({ ...edits, [a.name]: e.target.value })}
                  />
                )
              }
            />
          ))}
          {showOthers && (
            <Row
              center
              actionsWidth="w-64"
              title={<span className="flex items-center gap-1">{m.others}</span>}
              sub={m.othersHint}
              actions={<Input aria-label={m.others} className="w-64 max-md:w-40" placeholder="pg.web:5432" disabled={readOnly} value={others} onChange={(e) => setOthersEdit(e.target.value)} />}
            />
          )}
        </List>
      )}
    </Section>
  );
}

// ---- 项目连接 ----

// Links 列出已连接的项目，连接、断开其他项目。连接是双向的，用"容器.网络名"访问对方。
function Links({ project, services }: { project: Project; services: ServiceStatus[] }) {
  const m = t.projectNetwork.links;
  const queryClient = useQueryClient();
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: () => projectClient.listProjects({}),
  });
  const [open, setOpen] = useState(false);
  const [other, setOther] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const linked = new Set(project.links.map((l) => l.projectId));
  const candidates = (projects.data?.projects ?? []).map((p) => p.project!).filter((p) => p && p.id !== project.id && !linked.has(p.id));
  const selected = other || candidates[0]?.id || "";
  const addresses = intranetAddresses(project, services);
  // 访问示例用对方的第一个容器；还不知道时写占位。
  const exampleOf = (id: string, networkName: string) => {
    const first = projects.data?.projects.find((p) => p.project?.id === id)?.project?.containers[0]?.name;
    return first ? `${first}.${networkName}` : m.example(networkName);
  };
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["project", project.id] });
    queryClient.invalidateQueries({ queryKey: ["projects"] });
  };

  const connect = async () => {
    setBusy(true);
    setError("");
    try {
      await projectClient.connectProject({
        projectId: project.id,
        otherProjectId: selected,
      });
      setOpen(false);
      setOther("");
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const disconnect = async (id: string, name: string) => {
    if (!(await confirmAction(m.disconnectConfirm(name), { action: m.disconnect, danger: true }))) return;
    setError("");
    try {
      await projectClient.disconnectProject({
        projectId: project.id,
        otherProjectId: id,
      });
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const cancel = () => {
    setOpen(false);
    setError("");
  };

  return (
    <Section
      title={m.title}
      help={<HelpParts parts={[m.intro(project.networkName), m.help, ...(addresses.length > 0 ? [`${m.addressesIntro} ${addresses.map((a) => `${a.name}.${project.networkName}`).join(", ")}`] : [])]} />}
      actions={
        !open && (
          <Button variant="outline" onClick={() => setOpen(true)}>
            <Plus />
            {m.add}
          </Button>
        )
      }
    >
      {open &&
        (projects.isPending ? (
          <Skeleton className="h-8 w-full" />
        ) : projects.isError ? (
          <QueryError error={projects.error} onRetry={() => projects.refetch()} />
        ) : candidates.length === 0 ? (
          <div className="flex min-h-8 flex-wrap items-center gap-2">
            <p className="text-muted-foreground">{m.noCandidates}</p>
            <Button variant="ghost" onClick={cancel}>
              {t.common.actions.cancel}
            </Button>
          </div>
        ) : (
          <form
            className="flex flex-wrap items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              connect();
            }}
          >
            <div className="min-w-48 flex-1">
              <Select aria-label={m.project} className="w-full" value={selected} onChange={(e) => setOther(e.target.value)}>
                {candidates.map((p) => (
                  <option key={p.id} value={p.id}>
                    {m.option(p.name, p.networkName)}
                  </option>
                ))}
              </Select>
            </div>
            <Button type="submit" disabled={busy || !selected}>
              {busy && <Spinner />}
              {m.connect}
            </Button>
            <Button variant="ghost" onClick={cancel}>
              {t.common.actions.cancel}
            </Button>
          </form>
        ))}
      {error && <Alert>{error}</Alert>}
      {project.links.length === 0 ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : (
        <List>
          {project.links.map((l) => (
            <Row
              key={l.projectId}
              title={<span className="min-w-0 wrap-anywhere">{l.name}</span>}
              sub={`${l.networkName} · ${m.columns.example} ${exampleOf(l.projectId, l.networkName)}`}
              actions={
                <Button size="row" variant="ghost" onClick={() => disconnect(l.projectId, l.name)}>
                  {m.disconnect}
                </Button>
              }
            />
          ))}
        </List>
      )}
    </Section>
  );
}
