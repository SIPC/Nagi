import { useRole } from "@/components/role";
import { Fragment, useContext, useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowUpRight, Plus } from "lucide-react";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { errorMessage, orOther, knownNodes, nodeClient, projectClient } from "@/lib/api";
import { cn, formatDate } from "@/lib/utils";
import { t } from "@/i18n";
import { Alert, Button, ButtonLink, Check, Collapse, CopyButton, Dialog, DialogActions, EmptyRow, Field, FormRow, Input, List, Notice, QueryError, ReadOnlyContext, Row, RowMenu, RowsSkeleton, Section, Select, Skeleton, Spinner, Status, dotColor, textLink } from "@/components/ui";
import type { DomainCheck } from "@/gen/nagi/v1/api_pb";
import type { Container, Exposure, ExposureSchema, Project, ServiceStatus } from "@/gen/nagi/v1/common_pb";
import { confirmAction } from "@/components/confirm";
import { HelpParts } from "@/components/help-parts";

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

// InternalAccess：项目内的容器访问这个容器用的地址，连接的项目再加上网络名。
// 容器组还没运行时不知道里面的容器，占位的名字不是地址，不列出。
function InternalAccess({ project, container, services }: { project: Project; container: Container; services: ServiceStatus[] }) {
  const m = t.projectNetwork.access;
  const own = services.filter((s) => s.container === container.name);
  const names = intranetAddresses(project, services)
    .filter((a) => !a.placeholder && (container.kind === "group" ? a.name.endsWith(`.${container.name}`) : a.name === container.name))
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
        <EmptyRow>{m.empty}</EmptyRow>
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
      const prefix = `${manyDomains ? `${c.domain} ` : ""}${manyEntries ? `${orOther(e.nodeName)} ` : ""}`;
      const list: { text: string; danger?: boolean; cert?: boolean }[] = [];
      if (e.dns === "mismatch") list.push({ text: mismatchText(c) });
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
  return m.certExpires(formatDate(earliest) ?? "");
}

// mismatchText：没有「查看访问地址」时面板不给解析到的地址，只写结论。
function mismatchText(c: DomainCheck) {
  const m = t.projectNetwork.routes.check;
  return c.addresses.length > 0 ? m.dnsMismatch(t.common.list(c.addresses)) : m.dnsMismatchHidden;
}

// Addresses：映射的访问地址。plain 为 true 时是「节点名:端口」（没有「查看访问地址」，或者看不到节点的地址），只是文字，不是链接。
type Addresses = string[] & { plain?: boolean };

function useRouteNodes(project: Project) {
  const { can } = useRole();
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const nodeName = (id: string) => knownNodes(nodes.data).find((n) => n.id === id)?.name ?? orOther(id);
  // 映射开在它的入口节点上（默认是容器所在的节点），访问地址用这些节点的地址。
  const entriesOf = (r: Route) => {
    const own = project.containers.find((c) => c.name === r.container)?.nodeId ?? "";
    return [...new Set([...(r.noLocalEntry ? [] : [own]), ...r.entryNodes])].filter(Boolean);
  };
  // 有「查看访问地址」、拿得到节点地址时用 IP:端口；否则用节点名（看不到的节点是「其他节点」）。
  const addressesOf = (r: Route): Addresses => {
    const ids = [...new Set([...(r.noLocalEntry ? [] : [project.containers.find((c) => c.name === r.container)?.nodeId ?? ""]), ...r.entryNodes])];
    const real = ids.map((id) => nodes.data?.nodes.find((n) => n.id === id)?.address ?? "");
    if (can("project.addresses") && real.length > 0 && real.every(Boolean)) return real;
    return Object.assign([...new Set(ids.map(nodeName))], { plain: true });
  };
  return { nodes, nodeName, entriesOf, addressesOf };
}

// RouteTitle：第一行的访问地址。http 绑了域名用域名，否则用"节点地址:端口"；tcp/udp 只给地址，不是网页。
// 映射没生效或者不知道地址时写映射名。
function RouteTitle({ route: r, addresses, linkClassName }: { route: Route; addresses: Addresses; linkClassName?: string }) {
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
          r.protocol === "http" && !addresses.plain ? (
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
  const plain = r.domains.length > 0 ? (r.httpOnly ? m.httpOnlyBadge : certSummary(checks)) : hasAddress ? protocol : `${protocol}/${r.published}`;
  const last = r.proxyProtocol ? `${plain} · ${m.proxyProtocolBadge}` : plain;
  const entry = entries.length > 0 ? m.entryList(t.common.list([...entries].sort())) : "";
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
                  <button type="button" className={textLink} onClick={onReason}>
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
    <a href={url} target="_blank" rel="noreferrer" className={cn("inline-flex min-w-0 items-center gap-1 rounded-sm wrap-anywhere outline-none hover:underline focus-ring", className)}>
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

// RoutesOverview：项目页上全部端口映射的总览。编辑在容器页，整行链过去。
function RoutesOverview({ project }: { project: Project }) {
  const m = t.projectNetwork.routes;
  const { nodes, nodeName, entriesOf, addressesOf } = useRouteNodes(project);
  const domains = useDomainChecks(project.id, project.routes.filter((r) => r.domains.length > 0), 15_000);
  return (
    <Section title={m.title} help={m.overviewDescription}>
      {/* 两个查询都失败时只显示一条，重试两个都重新取。 */}
      {(nodes.isError || domains.error) && (
        <QueryError
          error={nodes.error ?? domains.error}
          onRetry={() => {
            if (nodes.isError) nodes.refetch();
            if (domains.error) domains.refetch();
          }}
        />
      )}
      {project.routes.length === 0 ? (
        // 添加在容器的「网络」标签，链到第一个容器，在那里可以换到别的容器。
        <EmptyRow
          action={
            project.containers.length > 0 && (
              <ButtonLink to="/projects/$projectId/c/$container" params={{ projectId: project.id, container: project.containers[0].name }} search={{ view: "network" }} variant="outline">
                <Plus />
                {m.add}
              </ButtonLink>
            )
          }
        >
          {m.empty}
        </EmptyRow>
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
                  className="absolute inset-0 rounded-md outline-none focus-ring"
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

// ServiceRoutes：服务页里指向这个服务的端口映射，只看不改；点一行到容器组的「网络」标签里修改。
export function ServiceRoutes({ project, container, service }: { project: Project; container: string; service: string }) {
  const routes = project.routes.filter((r) => r.container === container && r.service === service);
  const { nodes, nodeName, entriesOf, addressesOf } = useRouteNodes(project);
  const domains = useDomainChecks(project.id, routes.filter((r) => r.domains.length > 0), 15_000);
  if (routes.length === 0) return <EmptyRow>{t.projectNetwork.routes.empty}</EmptyRow>;
  if (nodes.isPending) return <RowsSkeleton count={Math.min(routes.length, 6)} />;
  return (
    <List>
      {routes.map((r) => {
        const checks = domains.checks.filter((c) => c.route === r.name);
        const addresses = addressesOf(r);
        return (
          <div key={r.name} className="group/route relative">
            <Link
              to="/projects/$projectId/c/$container"
              params={{ projectId: project.id, container: r.container }}
              search={{ view: "network" }}
              aria-label={r.name}
              className="absolute inset-0 rounded-md outline-none focus-ring"
            />
            <Row
              className="group-hover/route:bg-accent"
              title={<RouteTitle route={r} addresses={addresses} linkClassName="relative z-10" />}
              sub={<RouteSub route={r} checks={checks} entries={entriesOf(r).map(nodeName)} hasAddress={addresses.length > 0} />}
              cols={[{ content: <RouteStatus state={routeState(r, checks, domains.pending)} />, phone: true }]}
            />
          </div>
        );
      })}
    </List>
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
    proxyProtocol: false,
    localEntry: true,
    entryNodes: [] as string[],
  };
  const [form, setForm] = useState(empty);
  const [open, setOpen] = useState(false);
  const [formError, setFormError] = useState("");
  // 提交过一次以后才显示必填的提示，免得刚打开就报错。
  const [checked, setChecked] = useState(false);
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
  const targetError = checked && !form.target.trim() ? m.containerPortRequired : "";
  const publishedError = checked && !hasDomains && !form.published.trim() ? m.nodePortRequired : "";
  const draftRoute = {
    name: form.name,
    container,
    service: form.service || containerServices[0] || "",
    target: Number(form.target),
    protocol: form.protocol,
    published: hasDomains ? 0 : Number(form.published || 0),
    domains: domainList,
    httpOnly: hasDomains && form.httpOnly,
    proxyProtocol: !isHTTP && form.proxyProtocol,
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
    setChecked(false);
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
      proxyProtocol: r.proxyProtocol,
      localEntry: !r.noLocalEntry,
      entryNodes: r.entryNodes,
    });
    setFormError("");
    setChecked(false);
    setOpen(true);
  };

  const save = async () => {
    setChecked(true);
    if (!form.target.trim() || (!hasDomains && !form.published.trim())) return;
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
    if (!(await confirmAction(m.deleteConfirm(name), { action: m.deleteAction, danger: true }))) return;
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
      title={m.title}
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
      {(nodes.isError || savedDomains.error) && (
        <QueryError
          error={nodes.error ?? savedDomains.error}
          onRetry={() => {
            if (nodes.isError) nodes.refetch();
            if (savedDomains.error) savedDomains.refetch();
          }}
        />
      )}
      {routes.length === 0 ? (
        <EmptyRow
          action={
            <Button variant="outline" onClick={openAdd}>
              <Plus />
              {m.add}
            </Button>
          }
        >
          {m.empty}
        </EmptyRow>
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
                        {retrying === r.name && <Spinner />}
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
      <Dialog open={shownReasons.length > 0} onClose={() => setReasonOf("")} title={m.check.certErrorsTitle} className="max-w-[520px]">
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
            <Field label={m.containerPort} htmlFor="route-target" error={targetError}>
              <Input id="route-target" inputMode="numeric" autoComplete="off" spellCheck={false} placeholder="80" value={form.target} onChange={(e) => setForm({ ...form, target: e.target.value })} />
            </Field>
            <Field label={m.protocol} htmlFor="route-protocol">
              <Select id="route-protocol" value={form.protocol} onChange={(e) => setForm({ ...form, protocol: e.target.value })}>
                <option value="http">HTTP</option>
                <option value="tcp">TCP</option>
                <option value="udp">UDP</option>
                <option value="tcp+udp">TCP+UDP</option>
              </Select>
            </Field>
          </FormRow>
          <Collapse open={isHTTP} gap={4}>
            <Field label={m.domains} htmlFor="route-domains" help={m.domainsHelp}>
              <Input id="route-domains" autoComplete="off" autoCapitalize="none" spellCheck={false} placeholder="app.example.com" value={form.domains} onChange={(e) => setForm({ ...form, domains: e.target.value })} />
            </Field>
            {/* 各入口节点的解析结果：字段下面单独一行，不占格式说明的位置。 */}
            {hasDomains && (
              <div className="pt-2 leading-5 text-muted-foreground">
                <DraftDns checks={draftDomains.checks} error={draftDomains.error} />
              </div>
            )}
          </Collapse>
          <Collapse open={!hasDomains} gap={4}>
            <Field label={m.nodePort} htmlFor="route-published" help={m.nodePortHelp} error={publishedError}>
              <Input id="route-published" inputMode="numeric" autoComplete="off" spellCheck={false} placeholder="8080" value={form.published} onChange={(e) => setForm({ ...form, published: e.target.value })} />
            </Field>
          </Collapse>
          <Field label={m.entryNodes} help={m.entryNodesHelp}>
            {nodes.isPending ? (
              <Skeleton className="h-5 w-64 max-w-full" />
            ) : (
              <div role="group" aria-label={m.entryNodes} className="flex flex-wrap gap-x-6 gap-y-2">
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
            {hasDomains && multipleEntries && !form.httpOnly && <Notice tone="warning">{m.entryDomainWarningShort}</Notice>}
          </Field>
          <Collapse open={hasDomains} gap={4}>
            <Check className="justify-self-start" checked={form.httpOnly} onChange={(v) => setForm({ ...form, httpOnly: v })} help={m.httpOnlyHelp}>
              {m.httpOnly}
            </Check>
          </Collapse>
          <Collapse open={!isHTTP} gap={4}>
            <Check className="justify-self-start" checked={form.proxyProtocol} onChange={(v) => setForm({ ...form, proxyProtocol: v })} help={m.proxyProtocolHelp}>
              {m.proxyProtocol}
            </Check>
          </Collapse>
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
            {`${multiple ? `${c.domain} ` : ""}${orOther(e.nodeName)}: ${e.dns === "mismatch" ? mismatchText(c) : e.dns === "ok" ? m.dnsOk : e.dns === "unresolved" ? m.dnsUnresolved : m.dnsUnknown}`}
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

// Intranet 显示项目内网里的名字和它们的内部端口。一行一个名字：节点、补充的内部端口，「编辑」打开对话框，只改这一行。
// 接口存的是"名字:端口"的列表，界面上按名字分行；对不上任何一行的（容器已删除、容器组还没运行）放在「其他」，原样保留。
function Intranet({ project, services }: { project: Project; services: ServiceStatus[] }) {
  const m = t.projectNetwork.intranet;
  const queryClient = useQueryClient();
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const nodeName = (id: string) => knownNodes(nodes.data).find((n) => n.id === id)?.name ?? orOther(id);
  // editing：正在编辑的一行，名字；「其他」用空字符串。
  const [editing, setEditing] = useState<string | null>(null);
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
  const showOthers = savedOthers.length > 0 || addresses.some((a) => a.placeholder);

  // save：换掉一行的端口，其余行照原样。接口每次提交完整的设置：其余各项照原样带上（共享变量由面板保留）。
  const save = async (row: string, ports: string[]) => {
    const rows = { ...saved, [row]: ports };
    await projectClient.updateProjectSettings({
      projectId: project.id,
      baseRevision: project.revision,
      name: project.name,
      networkName: project.networkName,
      settings: {
        snapshotIntervalMinutes: project.settings?.snapshotIntervalMinutes ?? 0,
        snapshotLimit: project.settings?.snapshotLimit ?? 0,
        internalPorts: [...[...known].flatMap((name) => (rows[name] ?? []).map((p) => `${name}:${p}`)), ...(row === "" ? ports : savedOthers)],
      },
    });
    await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
  };

  const portsCol = (ports: string[]) => ({ content: ports.length > 0 ? <span title={ports.join(" ")}>{ports.join(" ")}</span> : m.noPorts, width: "w-40" });
  const editButton = (row: string) => (
    <Button size="row" variant="ghost" onClick={() => setEditing(row)}>
      {t.common.actions.edit}
    </Button>
  );
  const hasRows = addresses.length > 0 || showOthers;
  return (
    <Section
      title={m.title}
      help={<HelpParts parts={[m.description, m.help, m.portsRowHelp, m.internalPortsHelp]} />}
      actions={<span>{`${t.projectNetwork.links.networkName} ${project.networkName}`}</span>}
    >
      {nodes.isError && <QueryError error={nodes.error} onRetry={() => nodes.refetch()} />}
      {!hasRows ? (
        <EmptyRow
          action={
            <ButtonLink to="/projects/$projectId/add" params={{ projectId: project.id }} variant="outline">
              {m.addContainer}
            </ButtonLink>
          }
        >
          {m.empty}
        </EmptyRow>
      ) : (
        <List>
          {addresses.map((a) => (
            <Row
              key={a.name}
              title={<span className="min-w-0 wrap-anywhere">{a.name}</span>}
              cols={[{ content: nodes.isPending ? <Skeleton className="h-3 w-12" /> : <span title={nodeName(a.node)}>{nodeName(a.node)}</span>, width: "w-32" }, portsCol(saved[a.name] ?? [])]}
              actions={a.placeholder ? null : editButton(a.name)}
            />
          ))}
          {showOthers && <Row title={m.others} sub={m.othersHint} cols={[{ content: null, width: "w-32" }, portsCol(savedOthers)]} actions={editButton("")} />}
        </List>
      )}
      {editing !== null && (
        <PortsDialog
          title={editing === "" ? m.others : editing}
          initial={(editing === "" ? savedOthers : (saved[editing] ?? [])).join(" ")}
          others={editing === ""}
          onSave={(ports) => save(editing, ports)}
          onClose={() => setEditing(null)}
        />
      )}
    </Section>
  );
}

// PortsDialog：编辑一行的内部端口，空格分隔。
function PortsDialog({ title, initial, others, onSave, onClose }: { title: string; initial: string; others: boolean; onSave: (ports: string[]) => Promise<void>; onClose: () => void }) {
  const m = t.projectNetwork.intranet;
  const [value, setValue] = useState(initial);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    if (value.trim() === initial.trim()) return onClose();
    setBusy(true);
    setError("");
    try {
      await onSave(splitPorts(value));
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} title={m.editPorts(title)}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <Field label={m.internalPorts} htmlFor="intranet-ports" hint={others ? m.othersFormat : m.portsFormat} help={m.portsRowHelp}>
          <Input id="intranet-ports" value={value} onChange={(e) => setValue(e.target.value)} placeholder={others ? "pg.web:5432" : "80 9100"} autoComplete="off" spellCheck={false} />
        </Field>
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy}>
            {busy && <Spinner />}
            {t.common.actions.save}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
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
      help={<HelpParts parts={[m.intro(project.networkName), m.help, ...(addresses.length > 0 ? [`${m.addressesIntro} ${t.common.list(addresses.map((a) => `${a.name}.${project.networkName}`))}`] : [])]} />}
      actions={
        <Button variant="outline" onClick={() => setOpen(true)}>
          <Plus />
          {m.add}
        </Button>
      }
    >
      {/* 连接项目用对话框（ui.md：表单放进对话框），标题栏的按钮一直在。 */}
      <Dialog open={open} onClose={cancel} title={m.add}>
        {projects.isPending ? (
          <RowsSkeleton count={1} />
        ) : projects.isError ? (
          <QueryError error={projects.error} onRetry={() => projects.refetch()} />
        ) : candidates.length === 0 ? (
          <>
            <p className="text-muted-foreground">{m.noCandidates}</p>
            <DialogActions>
              <Button variant="outline" onClick={cancel}>
                {t.common.actions.cancel}
              </Button>
              <ButtonLink to="/projects/new">{t.projects.newProject}</ButtonLink>
            </DialogActions>
          </>
        ) : (
          <form
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              connect();
            }}
          >
            <Field label={m.project} htmlFor="link-project">
              <Select id="link-project" value={selected} onChange={(e) => setOther(e.target.value)}>
                {candidates.map((p) => (
                  <option key={p.id} value={p.id}>
                    {m.option(p.name, p.networkName)}
                  </option>
                ))}
              </Select>
            </Field>
            {error && <Alert>{error}</Alert>}
            <DialogActions>
              <Button variant="outline" onClick={cancel}>
                {t.common.actions.cancel}
              </Button>
              <Button type="submit" disabled={busy}>
                {busy && <Spinner />}
                {m.connect}
              </Button>
            </DialogActions>
          </form>
        )}
      </Dialog>
      {!open && error && <Alert>{error}</Alert>}
      {project.links.length === 0 ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : (
        <List>
          {project.links.map((l) => (
            // 对方不在访问范围内时没有名称，只能看，不能断开（见 docs/panel.md#访问范围）。
            <Row
              key={l.projectId}
              title={<span className={cn("min-w-0 wrap-anywhere", !l.name && "text-muted-foreground")}>{l.name || m.otherProject}</span>}
              sub={`${l.networkName} · ${m.exampleLabel} ${exampleOf(l.projectId, l.networkName)}`}
              actions={
                l.name ? (
                  <Button size="row" variant="ghost" onClick={() => disconnect(l.projectId, l.name)}>
                    {m.disconnect}
                  </Button>
                ) : null
              }
            />
          ))}
        </List>
      )}
    </Section>
  );
}
