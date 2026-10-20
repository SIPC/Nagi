import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink } from "lucide-react";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import { slugName } from "@/lib/utils";
import { Alert, Badge, Button, Check, EmptyRow, Field, Input, Notice, Section, Select, Spinner, Table, Td, Textarea, Th } from "@/components/ui";
import type { Project, ServiceStatus } from "@/gen/nagi/v1/common_pb";

// 「网络」标签页：端口映射与域名、项目内网、共享网络。

export function NetworkTab({ nodeId, project, services }: { nodeId: string; project: Project; services: ServiceStatus[] }) {
  return (
    <div className="grid gap-6">
      <RoutesSection nodeId={nodeId} project={project} services={services} />
      <NetworkSettings nodeId={nodeId} project={project} services={services} />
    </div>
  );
}

// ---- 端口映射 ----

function RoutesSection({ nodeId, project, services }: { nodeId: string; project: Project; services: ServiceStatus[] }) {
  const queryClient = useQueryClient();
  const empty = { name: "", container: "", service: "", target: "", protocol: "http", published: "", domains: "", httpOnly: false };
  const [form, setForm] = useState(empty);
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });
  const container = form.container || project.containers[0]?.name || "";
  const containerServices = [...new Set(services.filter((s) => s.container === container).map((s) => s.name))];
  const isGroup = project.containers.find((c) => c.name === container)?.kind === "group";
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  const address = nodes.data?.nodes.find((n) => n.id === nodeId)?.address ?? "";
  // 访问地址：http 绑了域名用域名，否则用"节点地址:端口"；tcp/udp 只给地址，不是网页。
  const accessURL = (r: (typeof project.routes)[number]) => {
    if (r.domains.length) return `${r.httpOnly ? "http" : "https"}://${r.domains[0]}`;
    if (!address || !r.published) return "";
    return r.protocol === "http" ? `http://${address}:${r.published}` : "";
  };

  const save = async () => {
    setError("");
    setBusy(true);
    try {
      await projectClient.saveNodeProjectRoute({
        nodeId,
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
    if (!confirm(`删除端口映射 ${name}？对应的端口和域名将立即停止对外提供访问。`)) return;
    setError("");
    try {
      await projectClient.deleteNodeProjectRoute({ nodeId, projectId: project.id, name });
      refresh();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Section
      flush
      title="端口映射"
      description="经 Caddy 对外开放容器端口或绑定域名，保存后立即生效。"
      help={`无需重新部署。配置文件位于 ${project.dir}/config/routes/，也可在节点上直接修改。compose 中新增的 ports 会在部署时自动导入。`}
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
            添加映射
          </Button>
        )
      }
    >
      {error && !open && <Alert className="mx-5 mb-3">{error}</Alert>}
      {project.routes.length === 0 ? (
        <EmptyRow>暂无端口映射</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>名称</Th>
              <Th>目标</Th>
              <Th>对外</Th>
              <Th>访问地址</Th>
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
                        {r.httpOnly && (
                          <Badge className="ml-2 font-sans">仅 HTTP</Badge>
                        )}
                      </Td>
                      <Td className="font-mono text-xs">
                        {url ? (
                          <a href={url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 hover:underline">
                            {url.replace(/^https?:\/\//, "")}
                            <ExternalLink className="size-3 text-muted-foreground" />
                          </a>
                        ) : address && r.published > 0 ? (
                          <span className="text-muted-foreground">
                            {address}:{r.published}
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
                      编辑
                    </Button>
                    <Button variant="ghost" size="xs" className="hover:text-destructive" onClick={() => remove(r.name)}>
                      删除
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
          className="mt-1 grid gap-4 border-t bg-subtle px-5 pt-4 pb-4"
          onSubmit={(e) => {
            e.preventDefault();
            save();
          }}
        >
          <div className="text-[13px] font-medium">{form.name ? `编辑映射 ${form.name}` : "添加映射"}</div>
          <div className="grid gap-4 sm:grid-cols-3 lg:grid-cols-6">
            <Field label="容器" htmlFor="route-container">
              <Select id="route-container" value={container} onChange={(e) => setForm({ ...form, container: e.target.value, service: "" })}>
                {project.containers.map((m) => (
                  <option key={m.name}>{m.name}</option>
                ))}
              </Select>
            </Field>
            {isGroup && (
              <Field label="组内容器" htmlFor="route-service">
                <Select id="route-service" value={form.service || containerServices[0] || ""} onChange={(e) => setForm({ ...form, service: e.target.value })}>
                  {containerServices.map((s) => (
                    <option key={s}>{s}</option>
                  ))}
                </Select>
              </Field>
            )}
            <Field label="容器端口" htmlFor="route-target">
              <Input id="route-target" required inputMode="numeric" className="font-mono" placeholder="80" value={form.target} onChange={(e) => setForm({ ...form, target: e.target.value })} />
            </Field>
            <Field label="协议" htmlFor="route-protocol">
              <Select id="route-protocol" value={form.protocol} onChange={(e) => setForm({ ...form, protocol: e.target.value })}>
                <option value="http">HTTP</option>
                <option value="tcp">TCP</option>
                <option value="udp">UDP</option>
              </Select>
            </Field>
            <Field label="节点端口" htmlFor="route-published" help="在节点上对外开放的端口。绑定域名时可留空，通过 80/443 访问。">
              <Input id="route-published" inputMode="numeric" className="font-mono" placeholder="8080" value={form.published} onChange={(e) => setForm({ ...form, published: e.target.value })} />
            </Field>
            <Field label="域名" htmlFor="route-domains" className={isGroup ? "" : "lg:col-span-2"} help="多个域名以逗号分隔。域名需解析到此节点，Caddy 自动申请证书。">
              <Input id="route-domains" className="font-mono" placeholder="app.example.com" value={form.domains} onChange={(e) => setForm({ ...form, domains: e.target.value })} />
            </Field>
          </div>
          {error && <Alert>{error}</Alert>}
          <div className="flex flex-wrap items-center justify-between gap-3">
            <Check checked={form.httpOnly} onChange={(v) => setForm({ ...form, httpOnly: v })}>
              <span className="text-[13px]">仅 HTTP，不申请证书</span>
            </Check>
            <div className="flex gap-2">
              <Button variant="ghost" size="sm" onClick={() => setOpen(false)}>
                取消
              </Button>
              <Button type="submit" size="sm" disabled={busy}>
                {busy && <Spinner />}
                保存
              </Button>
            </div>
          </div>
        </form>
      )}
    </Section>
  );
}

// ---- 项目内网与共享网络 ----

// defaultNetworkName 由项目名生成加入共享网络用的网络名（小写字母、数字和 -，不超过 32 个字符）。
// 项目名里没有能用的字符（例如全是中文）时返回空，让用户自己起一个：别人要用它访问本项目，ID 前缀不好记。
function defaultNetworkName(name: string) {
  return slugName(name, "");
}

// sharedAddresses 列出本项目的容器在共享网络里的地址：单个容器是"容器.网络名"，容器组里的是"容器.容器组.网络名"。
function sharedAddresses(project: Project, services: ServiceStatus[], networkName: string) {
  return project.containers.flatMap((c) => {
    if (c.kind !== "group") return [`${c.name}.${networkName}`];
    const names = [...new Set(services.filter((s) => s.container === c.name).map((s) => s.name))];
    return names.length ? names.map((s) => `${s}.${c.name}.${networkName}`) : [`<容器>.${c.name}.${networkName}`];
  });
}

// NetworkSettings 只修改网络相关的设置；名称和快照间隔照原样提交（接口每次提交完整的设置）。
function NetworkSettings({ nodeId, project, services }: { nodeId: string; project: Project; services: ServiceStatus[] }) {
  const queryClient = useQueryClient();
  const s = project.settings;
  const networks = useQuery({ queryKey: ["shared-networks", nodeId], queryFn: () => nodeClient.listNodeSharedNetworks({ nodeId }) });
  const [intranet, setIntranet] = useState(!s?.intranetDisabled);
  const [networkName, setNetworkName] = useState(s?.networkName ?? "");
  const [joined, setJoined] = useState<string[]>(s?.sharedNetworks ?? []);
  const [linkPorts, setLinkPorts] = useState((s?.linkPorts ?? []).join("\n"));
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  const effectiveName = networkName || defaultNetworkName(project.name);
  const save = async () => {
    setSaved(false);
    if (joined.length > 0 && !effectiveName) {
      setError("加入共享网络需要设置网络名（小写字母、数字和 -）。");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await projectClient.updateNodeProjectSettings({
        nodeId,
        projectId: project.id,
        baseRevision: project.revision,
        name: project.name,
        settings: {
          intranetDisabled: !intranet,
          networkName: joined.length > 0 ? effectiveName : networkName,
          sharedNetworks: joined,
          linkPorts: linkPorts
            .split(/[\n,]/)
            .map((x) => x.trim())
            .filter(Boolean),
          snapshotIntervalMinutes: s?.snapshotIntervalMinutes ?? 0,
        },
      });
      setSaved(true);
      queryClient.invalidateQueries({ queryKey: ["project", nodeId, project.id] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section title="网络" description="项目内网与共享网络设置，保存后立即生效。" help="修改不计入容器版本。开启或关闭项目内网时，单个容器会被重建（短暂重启）。">
      <div className="grid gap-6">
        <div className="grid gap-1.5">
          <Check checked={intranet} onChange={setIntranet}>
            启用项目内网
          </Check>
          <p className="pl-6 text-xs text-muted-foreground">项目内的容器以「容器名」或「容器.容器组」互相访问。</p>
        </div>

        <div className="grid gap-4 border-t pt-5">
          <div>
            <h3 className="flex items-center gap-1.5 text-[13px] font-semibold">共享网络</h3>
            <p className="mt-0.5 text-xs text-muted-foreground">与加入同一网络的其他项目互通；网络与其他节点相连时可跨节点访问。</p>
          </div>
          <Field label="网络名" htmlFor="net-name" hint="小写字母、数字和 -" help="本项目的容器在共享网络中的地址为「容器.网络名」。">
            <Input
              id="net-name"
              value={networkName}
              onChange={(e) => setNetworkName(e.target.value)}
              placeholder={defaultNetworkName(project.name) || "shop"}
              className="max-w-60 font-mono"
            />
          </Field>
          {s?.networkName && networkName !== s.networkName && (s.sharedNetworks.length ?? 0) > 0 && (
            <Notice tone="warning">
              修改网络名后，其他项目中通过「容器.{s.networkName}」访问本项目的地址需改为「容器.{effectiveName}」。
            </Notice>
          )}
          <div className="grid gap-1.5">
            <span className="text-[13px] font-medium">加入的网络</span>
            {networks.isPending ? (
              <Spinner className="text-muted-foreground" />
            ) : networks.data?.networks.length ? (
              <div className="divide-y rounded-md border">
                {networks.data.networks.map((n) => (
                  <label key={n.name} className="flex cursor-pointer items-start gap-3 px-3.5 py-2.5 hover:bg-subtle">
                    <input
                      type="checkbox"
                      className="mt-0.5"
                      checked={joined.includes(n.name)}
                      onChange={(e) => {
                        if (e.target.checked && !networkName) setNetworkName(defaultNetworkName(project.name));
                        setJoined(e.target.checked ? [...joined, n.name] : joined.filter((x) => x !== n.name));
                      }}
                    />
                    <span className="grid gap-0.5">
                      <span className="font-mono text-[13px]">{n.name}</span>
                      <span className="text-xs text-muted-foreground">
                        {n.subnet} · {n.projects.map((p) => `${p.projectName}（${p.networkName}）`).join("、") || "暂无项目"}
                        {n.peers.length > 0 && ` · 已连接 ${n.peers.map((p) => p.name).join("、")}`}
                      </span>
                    </span>
                  </label>
                ))}
              </div>
            ) : (
              <p className="text-[13px] text-muted-foreground">此节点暂无共享网络，可在节点页创建。</p>
            )}
          </div>
          {s?.networkName && (s.sharedNetworks.length ?? 0) > 0 && (
            <div className="grid gap-1.5">
              <span className="text-[13px] font-medium">本项目的访问地址</span>
              <div className="flex flex-wrap gap-1.5">
                {sharedAddresses(project, services, s.networkName).map((a) => (
                  <code key={a} className="rounded-sm border bg-subtle px-1.5 py-0.5 font-mono text-xs">
                    {a}
                  </code>
                ))}
              </div>
            </div>
          )}
          <Field
            label="跨节点端口"
            htmlFor="net-linkports"
            hint="每行一个，如 pg:5432；容器组内写 容器.容器组:端口"
            help="相连节点经代理访问本项目时，需额外开放的容器未声明的端口。"
          >
            <Textarea id="net-linkports" value={linkPorts} onChange={(e) => setLinkPorts(e.target.value)} spellCheck={false} className="min-h-16 max-w-md font-mono text-[13px]" />
          </Field>
        </div>

        {error && <Alert>{error}</Alert>}
        <div className="flex items-center justify-end gap-3 border-t pt-4">
          {saved && <span className="mr-auto text-[13px] text-muted-foreground">已保存</span>}
          <Button size="sm" onClick={save} disabled={busy}>
            {busy && <Spinner />}
            保存
          </Button>
        </div>
      </div>
    </Section>
  );
}
