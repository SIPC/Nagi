import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink } from "lucide-react";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import { t } from "@/i18n";
import { Alert, Badge, Button, Check, EmptyRow, Field, Input, Section, Select, Spinner, Table, Td, Textarea, Th } from "@/components/ui";
import type { Project, ServiceStatus } from "@/gen/nagi/v1/common_pb";

// 「网络」标签页：端口映射与域名、项目内网、项目连接（见 docs/agent.md#项目连接）。

export function NetworkTab({ project, services }: { project: Project; services: ServiceStatus[] }) {
  return (
    <div className="grid gap-6">
      <RoutesSection project={project} services={services} />
      <Intranet project={project} services={services} />
      <Links project={project} services={services} />
    </div>
  );
}

// ---- 端口映射 ----

function RoutesSection({ project, services }: { project: Project; services: ServiceStatus[] }) {
  const queryClient = useQueryClient();
  const empty = {
    name: "",
    container: "",
    service: "",
    target: "",
    protocol: "http",
    published: "",
    domains: "",
    httpOnly: false,
  };
  const [form, setForm] = useState(empty);
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["project", project.id] });
  const container = form.container || project.containers[0]?.name || "";
  const containerServices = [...new Set(services.filter((s) => s.container === container).map((s) => s.name))];
  const isGroup = project.containers.find((c) => c.name === container)?.kind === "group";
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  // 映射开在容器所在的节点上，访问地址用那个节点的地址。
  const nodeOf = (r: (typeof project.routes)[number]) => project.containers.find((c) => c.name === r.container)?.nodeId ?? "";
  const addressOf = (r: (typeof project.routes)[number]) => nodes.data?.nodes.find((n) => n.id === nodeOf(r))?.address ?? "";
  // 访问地址：http 绑了域名用域名，否则用"节点地址:端口"；tcp/udp 只给地址，不是网页。
  const accessURL = (r: (typeof project.routes)[number]) => {
    if (r.domains.length) return `${r.httpOnly ? "http" : "https"}://${r.domains[0]}`;
    const address = addressOf(r);
    if (!address || !r.published) return "";
    return r.protocol === "http" ? `http://${address}:${r.published}` : "";
  };

  const save = async () => {
    setError("");
    setBusy(true);
    try {
      await projectClient.saveRoute({
        projectId: project.id,
        route: {
          name: form.name,
          container,
          service: form.service || containerServices[0] || "",
          target: Number(form.target),
          protocol: form.protocol,
          published: Number(form.published || 0),
          domains: form.domains.split(/[\s,]+/).filter(Boolean),
          httpOnly: form.httpOnly,
        },
      });
      setForm(empty);
      setOpen(false);
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (name: string) => {
    if (!confirm(t.projectNetwork.routes.deleteConfirm(name))) return;
    setError("");
    try {
      await projectClient.deleteRoute({ projectId: project.id, name });
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Section
      flush
      title={t.projectNetwork.routes.title}
      description={t.projectNetwork.routes.description}
      help={t.projectNetwork.routes.help}
      actions={
        !open && (
          <Button
            size="sm"
            variant="outline"
            disabled={project.containers.length === 0}
            onClick={() => {
              setForm(empty);
              setOpen(true);
            }}
          >
            {t.projectNetwork.routes.add}
          </Button>
        )
      }
    >
      {error && !open && <Alert className="mb-3">{error}</Alert>}
      {project.routes.length === 0 ? (
        <EmptyRow>{t.projectNetwork.routes.empty}</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>{t.projectNetwork.routes.columns.name}</Th>
              <Th>{t.projectNetwork.routes.columns.target}</Th>
              <Th>{t.projectNetwork.routes.columns.published}</Th>
              <Th>{t.projectNetwork.routes.columns.url}</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {project.routes.map((r) => {
              const url = accessURL(r);
              return (
                <tr key={r.name}>
                  <Td className="font-mono text-xs">{r.name}</Td>
                  {r.error ? (
                    <Td colSpan={3} className="text-[13px] text-destructive">
                      {r.error}
                    </Td>
                  ) : (
                    <>
                      <Td className="font-mono text-xs">
                        {r.container}
                        {r.service !== r.container && `/${r.service}`}:{r.target}
                      </Td>
                      <Td className="font-mono text-xs">
                        {r.domains.length ? r.domains.join(", ") : `${r.protocol}/${r.published}`}
                        {r.httpOnly && <Badge className="ml-2 font-sans">{t.projectNetwork.routes.httpOnlyBadge}</Badge>}
                      </Td>
                      <Td className="font-mono text-xs">
                        {url ? (
                          <a href={url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 hover:underline">
                            {url.replace(/^https?:\/\//, "")}
                            <ExternalLink className="size-3 text-muted-foreground" />
                          </a>
                        ) : addressOf(r) && r.published > 0 ? (
                          <span className="text-muted-foreground">
                            {addressOf(r)}:{r.published}
                          </span>
                        ) : (
                          <span className="text-muted-foreground">—</span>
                        )}
                      </Td>
                    </>
                  )}
                  <Td className="text-right whitespace-nowrap">
                    <Button
                      variant="ghost"
                      size="xs"
                      onClick={() => {
                        setForm({
                          name: r.name,
                          container: r.container,
                          service: r.service,
                          target: String(r.target),
                          protocol: r.protocol || "http",
                          published: r.published ? String(r.published) : "",
                          domains: r.domains.join(", "),
                          httpOnly: r.httpOnly,
                        });
                        setOpen(true);
                      }}
                    >
                      {t.common.actions.edit}
                    </Button>
                    <Button variant="ghost" size="xs" className="hover:text-destructive" onClick={() => remove(r.name)}>
                      {t.common.actions.delete}
                    </Button>
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}
      {open && (
        <form
          className="mt-1 grid gap-4 rounded-md bg-subtle p-4"
          onSubmit={(e) => {
            e.preventDefault();
            save();
          }}
        >
          <div className="text-[13px] font-medium">{form.name ? t.projectNetwork.routes.edit(form.name) : t.projectNetwork.routes.add}</div>
          <div className="grid gap-4 sm:grid-cols-3 lg:grid-cols-6">
            <Field label={t.projectNetwork.routes.container} htmlFor="route-container">
              <Select id="route-container" value={container} onChange={(e) => setForm({ ...form, container: e.target.value, service: "" })}>
                {project.containers.map((m) => (
                  <option key={m.name}>{m.name}</option>
                ))}
              </Select>
            </Field>
            {isGroup && (
              <Field label={t.projectNetwork.routes.groupService} htmlFor="route-service">
                <Select id="route-service" value={form.service || containerServices[0] || ""} onChange={(e) => setForm({ ...form, service: e.target.value })}>
                  {containerServices.map((s) => (
                    <option key={s}>{s}</option>
                  ))}
                </Select>
              </Field>
            )}
            <Field label={t.projectNetwork.routes.containerPort} htmlFor="route-target">
              <Input
                id="route-target"
                required
                inputMode="numeric"
                className="font-mono"
                placeholder="80"
                value={form.target}
                onChange={(e) => setForm({ ...form, target: e.target.value })}
              />
            </Field>
            <Field label={t.projectNetwork.routes.protocol} htmlFor="route-protocol">
              <Select id="route-protocol" value={form.protocol} onChange={(e) => setForm({ ...form, protocol: e.target.value })}>
                <option value="http">HTTP</option>
                <option value="tcp">TCP</option>
                <option value="udp">UDP</option>
              </Select>
            </Field>
            <Field label={t.projectNetwork.routes.nodePort} htmlFor="route-published" help={t.projectNetwork.routes.nodePortHelp}>
              <Input
                id="route-published"
                inputMode="numeric"
                className="font-mono"
                placeholder="8080"
                value={form.published}
                onChange={(e) => setForm({ ...form, published: e.target.value })}
              />
            </Field>
            <Field
              label={t.projectNetwork.routes.domains}
              htmlFor="route-domains"
              className={isGroup ? "" : "lg:col-span-2"}
              help={t.projectNetwork.routes.domainsHelp}
            >
              <Input
                id="route-domains"
                className="font-mono"
                placeholder="app.example.com"
                value={form.domains}
                onChange={(e) => setForm({ ...form, domains: e.target.value })}
              />
            </Field>
          </div>
          {error && <Alert>{error}</Alert>}
          <div className="flex flex-wrap items-center justify-between gap-3">
            <Check checked={form.httpOnly} onChange={(v) => setForm({ ...form, httpOnly: v })}>
              <span className="text-[13px]">{t.projectNetwork.routes.httpOnly}</span>
            </Check>
            <div className="flex gap-2">
              <Button variant="ghost" size="sm" onClick={() => setOpen(false)}>
                {t.common.actions.cancel}
              </Button>
              <Button type="submit" size="sm" disabled={busy}>
                {busy && <Spinner />}
                {t.common.actions.save}
              </Button>
            </div>
          </div>
        </form>
      )}
    </Section>
  );
}

// ---- 项目内网 ----

// intranetAddresses 列出项目里每个容器在项目内网里的名字：单个容器是"容器名"，容器组里的是"容器.容器组"。
function intranetAddresses(project: Project, services: ServiceStatus[]) {
  return project.containers.flatMap((c) => {
    if (c.kind !== "group") return [{ name: c.name, node: c.nodeId }];
    const names = [...new Set(services.filter((s) => s.container === c.name).map((s) => s.name))];
    return (names.length ? names : [t.projectNetwork.intranet.servicePlaceholder]).map((s) => ({
      name: `${s}.${c.name}`,
      node: c.nodeId,
    }));
  });
}

// Intranet 显示项目内网里的名字，修改内部端口（只改这一项，其余设置照原样提交）。
function Intranet({ project, services }: { project: Project; services: ServiceStatus[] }) {
  const queryClient = useQueryClient();
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
  });
  const nodeName = (id: string) => nodes.data?.nodes.find((n) => n.id === id)?.name ?? id;
  const [ports, setPorts] = useState((project.settings?.internalPorts ?? []).join("\n"));
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const addresses = intranetAddresses(project, services);

  const save = async () => {
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      await projectClient.updateProjectSettings({
        projectId: project.id,
        baseRevision: project.revision,
        name: project.name,
        networkName: project.networkName,
        settings: {
          snapshotIntervalMinutes: project.settings?.snapshotIntervalMinutes ?? 0,
          internalPorts: ports
            .split(/[\n,]/)
            .map((x) => x.trim())
            .filter(Boolean),
        },
      });
      setSaved(true);
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section
      title={t.projectNetwork.intranet.title}
      description={t.projectNetwork.intranet.description}
      help={t.projectNetwork.intranet.help}
    >
      <div className="grid gap-5">
        {addresses.length === 0 ? (
          <p className="text-[13px] text-muted-foreground">{t.projectNetwork.intranet.empty}</p>
        ) : (
          <div>
            <Table>
              <thead>
                <tr>
                  <Th>{t.projectNetwork.intranet.columns.name}</Th>
                  <Th>{t.projectNetwork.intranet.columns.node}</Th>
                </tr>
              </thead>
              <tbody>
                {addresses.map((a) => (
                  <tr key={a.name}>
                    <Td className="font-mono text-xs">{a.name}</Td>
                    <Td className="text-[13px] text-muted-foreground">{nodeName(a.node)}</Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </div>
        )}
        <Field
          label={t.projectNetwork.intranet.internalPorts}
          htmlFor="net-internal-ports"
          hint={t.projectNetwork.intranet.internalPortsHint}
          help={t.projectNetwork.intranet.internalPortsHelp}
        >
          <Textarea
            id="net-internal-ports"
            value={ports}
            onChange={(e) => setPorts(e.target.value)}
            spellCheck={false}
            className="min-h-16 max-w-md font-mono text-[13px]"
          />
        </Field>
        {error && <Alert>{error}</Alert>}
        <div className="flex items-center justify-end gap-3 pt-2">
          {saved && <span className="mr-auto text-[13px] text-muted-foreground">{t.common.feedback.saved}</span>}
          <Button size="sm" onClick={save} disabled={busy}>
            {busy && <Spinner />}
            {t.common.actions.save}
          </Button>
        </div>
      </div>
    </Section>
  );
}

// ---- 项目连接 ----

// Links 列出已连接的项目，连接、断开其他项目。连接是双向的，用"容器.网络名"访问对方。
function Links({ project, services }: { project: Project; services: ServiceStatus[] }) {
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
    if (!confirm(t.projectNetwork.links.disconnectConfirm(name))) return;
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

  return (
    <Section
      flush
      title={t.projectNetwork.links.title}
      description={t.projectNetwork.links.description}
      help={t.projectNetwork.links.help}
      actions={
        !open && (
          <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
            {t.projectNetwork.links.add}
          </Button>
        )
      }
    >
      {error && !open && <Alert className="mb-3">{error}</Alert>}
      {project.links.length === 0 ? (
        <EmptyRow>{t.projectNetwork.links.empty}</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>{t.projectNetwork.links.columns.project}</Th>
              <Th>{t.projectNetwork.links.columns.networkName}</Th>
              <Th>{t.projectNetwork.links.columns.example}</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {project.links.map((l) => (
              <tr key={l.projectId}>
                <Td className="font-medium">{l.name}</Td>
                <Td className="font-mono text-xs">{l.networkName}</Td>
                <Td className="font-mono text-xs text-muted-foreground">{t.projectNetwork.links.example(l.networkName)}</Td>
                <Td className="text-right">
                  <Button size="sm" variant="ghost" onClick={() => disconnect(l.projectId, l.name)}>
                    {t.projectNetwork.links.disconnect}
                  </Button>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {addresses.length > 0 && (
        <div className="border-t py-4">
          <p className="mb-2 text-[13px] text-muted-foreground">{t.projectNetwork.links.addressesIntro}</p>
          <div className="flex flex-wrap gap-2">
            {addresses.map((a) => (
              <Badge key={a.name} className="font-mono">
                {a.name}.{project.networkName}
              </Badge>
            ))}
          </div>
        </div>
      )}
      {open && (
        <form
          className="grid gap-4 border-t py-4"
          onSubmit={(e) => {
            e.preventDefault();
            connect();
          }}
        >
          {candidates.length === 0 ? (
            <p className="text-[13px] text-muted-foreground">{t.projectNetwork.links.noCandidates}</p>
          ) : (
            <Field label={t.projectNetwork.links.project} htmlFor="link-project">
              <Select id="link-project" value={selected} onChange={(e) => setOther(e.target.value)} className="max-w-sm">
                {candidates.map((p) => (
                  <option key={p.id} value={p.id}>
                    {t.projectNetwork.links.option(p.name, p.networkName)}
                  </option>
                ))}
              </Select>
            </Field>
          )}
          {error && <Alert>{error}</Alert>}
          <div className="flex justify-end gap-2">
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={() => {
                setOpen(false);
                setError("");
              }}
            >
              {t.common.actions.cancel}
            </Button>
            <Button type="submit" size="sm" disabled={busy || !selected}>
              {busy && <Spinner />}
              {t.projectNetwork.links.connect}
            </Button>
          </div>
        </form>
      )}
    </Section>
  );
}
