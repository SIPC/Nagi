import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Code, ConnectError } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import { toast } from "sonner";
import { AlertRuleSchema, type AlertOverride, type AlertPolicy, type AlertRule } from "@/gen/nagi/v1/common_pb";
import type { AlertNoticeDef, AlertRuleDef, GetAlertRulesResponse } from "@/gen/nagi/v1/alert_state_pb";
import { alertClient, errorMessage, nodeClient, projectClient } from "@/lib/api";
import { useRole } from "@/components/role";
import { confirmAction } from "@/components/confirm";
import { withElevation } from "@/components/elevate";
import { useSearch } from "@tanstack/react-router";
import { Alert, Badge, Button, ButtonLink, Check, Dialog, DialogActions, Field, FormRow, Input, List, QueryError, Row, RowsSkeleton, Section, Select, SettingRow, SettingRows, SettingSwitch, Spinner } from "@/components/ui";
import { locale, locales, t } from "@/i18n";

// 设置 → 告警 的规则和事件通知（见 docs/monitoring.md#告警）。规则、字段和通知目录都由面板给（GetAlertRules 的 defs、notices），
// 这里不重复一份。覆盖按字段继承：全局 → 节点；全局 → 项目 → 容器 → 映射。每一层只存改过的字段，界面上没改的显示「跟随」。

const m = t.alerts.policy;

type Scope = "global" | "node" | "project" | "container" | "route";

type Target = { scope: Scope; nodeId: string; projectId: string; container: string; route: string };

const emptyTarget: Target = { scope: "global", nodeId: "", projectId: "", container: "", route: "" };

type FieldKey = "threshold" | "for_seconds" | "recover_threshold" | "recover_seconds" | "repeat_minutes" | "window_minutes" | "min_requests";

const fieldProp: Record<FieldKey, keyof AlertRule> = {
  threshold: "threshold",
  for_seconds: "forSeconds",
  recover_threshold: "recoverThreshold",
  recover_seconds: "recoverSeconds",
  repeat_minutes: "repeatMinutes",
  window_minutes: "windowMinutes",
  min_requests: "minRequests",
};

const ruleName = (name: string) => m.rule[name] ?? name;

// merge：按字段盖上去（undefined 表示这一层没写）。
function merge(...layers: (AlertRule | undefined)[]): AlertRule {
  const out = create(AlertRuleSchema);
  for (const l of layers) {
    if (!l) continue;
    if (l.enabled !== undefined) out.enabled = l.enabled;
    for (const p of Object.values(fieldProp)) {
      const v = l[p];
      if (v !== undefined) (out as Record<string, unknown>)[p] = v;
    }
  }
  return out;
}

const fieldCount = (r: AlertRule | undefined) => (r ? (r.enabled !== undefined ? 1 : 0) + Object.values(fieldProp).filter((p) => r[p] !== undefined).length : 0);

function unitText(d: AlertRuleDef, f: FieldKey): string {
  if (f === "threshold" || f === "recover_threshold") return m.units[d.unit] ?? "";
  return "";
}

function valueText(d: AlertRuleDef, r: AlertRule): string {
  if (r.enabled === false) return m.off;
  const parts: string[] = [];
  if (r.threshold !== undefined && d.fields.includes("threshold")) parts.push(`${d.lower ? "<" : ">"} ${r.threshold}${spaceUnit(m.units[d.unit])}`);
  if (r.forSeconds && d.fields.includes("for_seconds")) parts.push(duration(r.forSeconds));
  if (r.recoverThreshold !== undefined && d.fields.includes("recover_threshold")) parts.push(`↺ ${r.recoverThreshold}${spaceUnit(m.units[d.unit])}`);
  return parts.join(" · ") || m.on;
}

const spaceUnit = (u: string | undefined) => (!u ? "" : u === m.units.percent || u === m.units.ratio ? u : ` ${u}`);

function duration(sec: number): string {
  if (sec % 3600 === 0) return m.dur.h(sec / 3600);
  if (sec % 60 === 0) return m.dur.min(sec / 60);
  return m.dur.s(sec);
}

// rulesFor：这一层能设置的规则。
function defsFor(defs: AlertRuleDef[], scope: Scope): AlertRuleDef[] {
  switch (scope) {
    case "global":
      return defs;
    case "node":
      return defs.filter((d) => d.node && !d.panel);
    case "project":
    case "container":
      return defs.filter((d) => !d.node && !d.panel);
    case "route":
      return defs.filter((d) => d.route);
  }
}

function groupOf(d: AlertRuleDef): keyof typeof m.groups {
  if (d.panel) return "panel";
  if (d.node) return "node";
  if (d.route) return "route";
  return "container";
}

export function AlertRulesSettings() {
  const { can } = useRole();
  const rules = useQuery({ queryKey: ["alert-rules"], queryFn: () => alertClient.getAlertRules({}) });
  if (rules.isPending)
    return (
      <Section title={m.rulesTitle}>
        <RowsSkeleton count={6} />
      </Section>
    );
  if (!rules.data)
    return (
      <Section title={m.rulesTitle}>
        <QueryError error={errorMessage(rules.error)} onRetry={() => rules.refetch()} />
      </Section>
    );
  const d = rules.data;
  return (
    <>
      <RulesSection data={d} canNodes={can("node.manage")} canProjects={can("project.alerts") || can("project.manage")} />
      {d.canEditPolicy && <GeneralSection policy={d.policy!} builtin={d.builtin!} />}
      {(d.canEditNotices || d.canEditSecurityNotices) && <NoticesSection data={d} />}
    </>
  );
}

// ---- 规则 ----

function RulesSection({ data, canNodes, canProjects }: { data: GetAlertRulesResponse; canNodes: boolean; canProjects: boolean }) {
  const scopes: Scope[] = [];
  if (data.canEditPolicy) scopes.push("global");
  if (canNodes) scopes.push("node");
  if (canProjects) scopes.push("project", "container", "route");
  // 从节点、项目、容器页跳过来时（?scope=…），预先选好对象。
  const search = useSearch({ from: "/app/settings" });
  const [target, setTarget] = useState<Target>(() => {
    const s = search.scope as Scope | undefined;
    if (s && scopes.includes(s)) return { scope: s, nodeId: search.node ?? "", projectId: search.project ?? "", container: search.container ?? "", route: search.route ?? "" };
    return { ...emptyTarget, scope: scopes[0] ?? "global" };
  });
  const [editing, setEditing] = useState<AlertRuleDef | null>(null);
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), enabled: canNodes });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}), enabled: canProjects });
  const project = projects.data?.projects.find((p) => p.project?.id === target.projectId)?.project;
  const containers = project?.containers ?? [];
  const routes = (project?.routes ?? []).filter((r) => r.name && (target.container === "" || r.container === target.container));

  const global = data.policy?.rules ?? {};
  const builtin = data.builtin?.rules ?? {};
  const find = (o: Partial<AlertOverride>) =>
    data.overrides.find((x) => x.scope === o.scope && x.nodeId === (o.nodeId ?? "") && x.projectId === (o.projectId ?? "") && x.container === (o.container ?? "") && x.route === (o.route ?? ""));
  // 本层和上面各层（从下往上：内置、全局、项目、容器）。
  const layers = useMemo(() => {
    const above: { label: string; rules: Record<string, AlertRule> }[] = [{ label: m.chain.global, rules: global }];
    let own: AlertOverride | undefined;
    switch (target.scope) {
      case "node":
        own = find({ scope: "node", nodeId: target.nodeId });
        break;
      case "project":
        own = find({ scope: "project", projectId: target.projectId });
        break;
      case "container":
        above.push({ label: m.chain.project, rules: find({ scope: "project", projectId: target.projectId })?.rules ?? {} });
        own = find({ scope: "container", projectId: target.projectId, container: target.container });
        break;
      case "route":
        above.push({ label: m.chain.project, rules: find({ scope: "project", projectId: target.projectId })?.rules ?? {} });
        above.push({ label: m.chain.container, rules: find({ scope: "container", projectId: target.projectId, container: target.container })?.rules ?? {} });
        own = find({ scope: "route", projectId: target.projectId, route: target.route });
        break;
    }
    return { above, own };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data, target]);

  const ready =
    target.scope === "global" ||
    (target.scope === "node" && !!target.nodeId) ||
    (target.scope === "project" && !!target.projectId) ||
    (target.scope === "container" && !!target.projectId && !!target.container) ||
    (target.scope === "route" && !!target.projectId && !!target.container && !!target.route);
  const defs = defsFor(data.defs, target.scope);
  const ownRules: Record<string, AlertRule> = target.scope === "global" ? global : (layers.own?.rules ?? {});
  const inheritedOf = (name: string) =>
    merge(builtin[name], ...(target.scope === "global" ? [] : layers.above.map((l) => l.rules[name])));
  const effective = (name: string) => merge(inheritedOf(name), ownRules[name]);
  const overriddenCount = Object.values(ownRules).reduce((n, r) => n + fieldCount(r), 0);

  const groups = (["node", "container", "route", "panel"] as const).map((g) => ({ g, items: defs.filter((d) => groupOf(d) === g) })).filter((x) => x.items.length > 0);
  const objectName = () => {
    switch (target.scope) {
      case "node":
        return nodes.data?.nodes.find((n) => n.id === target.nodeId)?.name ?? target.nodeId;
      case "project":
        return project?.name ?? target.projectId;
      case "container":
        return `${project?.name ?? ""} / ${target.container}`;
      case "route":
        return `${project?.name ?? ""} / ${target.route}`;
    }
    return m.global;
  };

  const queryClient = useQueryClient();
  const saveLayer = async (next: Record<string, AlertRule>) => {
    try {
      if (target.scope === "global") {
        await alertClient.updateAlertPolicy({ policy: { ...data.policy!, rules: next }, baseRevision: data.policy!.revision });
      } else {
        await alertClient.setAlertOverride({
          scope: target.scope,
          nodeId: target.scope === "node" ? target.nodeId : "",
          projectId: target.scope === "node" ? "" : target.projectId,
          container: target.scope === "container" ? target.container : "",
          route: target.scope === "route" ? target.route : "",
          rules: next,
          baseRevision: layers.own?.revision ?? 0n,
        });
      }
      toast.success(m.saved);
    } catch (err) {
      if (err instanceof ConnectError && err.code === Code.Aborted) toast.error(m.conflict);
      throw err;
    } finally {
      await queryClient.invalidateQueries({ queryKey: ["alert-rules"] });
    }
  };

  if (scopes.length === 0) return null;
  return (
    <Section title={m.rulesTitle} help={m.rulesHelp}>
      <div className="grid gap-4 md:grid-cols-[repeat(auto-fill,minmax(180px,1fr))]">
        <Field label={m.scope} htmlFor="alert-scope">
          <Select id="alert-scope" value={target.scope} onChange={(e) => setTarget({ ...emptyTarget, scope: e.target.value as Scope })}>
            {scopes.map((s) => (
              <option key={s} value={s}>
                {s === "global" ? m.global : m[s]}
              </option>
            ))}
          </Select>
        </Field>
        {target.scope === "node" && (
          <Field label={m.node} htmlFor="alert-node">
            <Select id="alert-node" value={target.nodeId} onChange={(e) => setTarget({ ...target, nodeId: e.target.value })}>
              <option value="">{m.pickNode}</option>
              {(nodes.data?.nodes ?? []).map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name || n.id}
                </option>
              ))}
            </Select>
          </Field>
        )}
        {(target.scope === "project" || target.scope === "container" || target.scope === "route") && (
          <Field label={m.project} htmlFor="alert-project">
            <Select id="alert-project" value={target.projectId} onChange={(e) => setTarget({ ...target, projectId: e.target.value, container: "", route: "" })}>
              <option value="">{m.pickProject}</option>
              {(projects.data?.projects ?? []).map((p) => (
                <option key={p.project?.id} value={p.project?.id ?? ""}>
                  {p.project?.name}
                </option>
              ))}
            </Select>
          </Field>
        )}
        {(target.scope === "container" || target.scope === "route") && target.projectId && (
          <Field label={m.container} htmlFor="alert-container">
            <Select id="alert-container" value={target.container} onChange={(e) => setTarget({ ...target, container: e.target.value, route: "" })}>
              <option value="">{m.pickContainer}</option>
              {containers.map((c) => (
                <option key={c.name} value={c.name}>
                  {c.displayName || c.name}
                </option>
              ))}
            </Select>
          </Field>
        )}
        {target.scope === "route" && target.container && (
          <Field label={m.route} htmlFor="alert-route">
            <Select id="alert-route" value={target.route} onChange={(e) => setTarget({ ...target, route: e.target.value })}>
              <option value="">{m.pickRoute}</option>
              {routes.map((r) => (
                <option key={r.name} value={r.name}>
                  {`${r.name} (${r.protocol}${r.published ? ` :${r.published}` : ""})`}
                </option>
              ))}
            </Select>
          </Field>
        )}
      </div>
      {ready && (
        <>
          {target.scope !== "global" && (
            <div className="flex flex-wrap items-center justify-between gap-2 text-muted-foreground">
              <span>{overriddenCount > 0 ? m.overridden(overriddenCount) : m.inherited(layers.above[layers.above.length - 1].label)}</span>
              {overriddenCount > 0 && (
                <Button
                  variant="ghost"
                  onClick={async () => {
                    if (!(await confirmAction(m.resetLayerConfirm(objectName()), { action: m.resetLayer, danger: true }))) return;
                    try {
                      await saveLayer({});
                    } catch (err) {
                      toast.error(errorMessage(err));
                    }
                  }}
                >
                  {m.resetLayer}
                </Button>
              )}
            </div>
          )}
          {groups.map(({ g, items }) => (
            <div key={g} className="grid gap-2">
              {groups.length > 1 && <h4 className="font-medium">{m.groups[g]}</h4>}
              <List>
                {items.map((d) => {
                  const eff = effective(d.name);
                  const n = fieldCount(ownRules[d.name]);
                  return (
                    <Row
                      key={d.name}
                      center
                      actionsWidth="w-auto"
                      title={
                        <span className="flex min-w-0 items-center gap-2">
                          <span className="truncate">{ruleName(d.name)}</span>
                          {target.scope !== "global" && n > 0 && <Badge tone="info">{m.overridden(n)}</Badge>}
                        </span>
                      }
                      sub={<span className={eff.enabled === false ? "text-muted-foreground" : undefined}>{valueText(d, eff)}</span>}
                      actions={
                        <Button variant="outline" onClick={() => setEditing(d)}>
                          {m.edit}
                        </Button>
                      }
                    />
                  );
                })}
              </List>
            </div>
          ))}
        </>
      )}
      {editing && (
        <RuleDialog
          def={editing}
          scope={target.scope}
          own={ownRules[editing.name]}
          inherited={inheritedOf(editing.name)}
          from={target.scope === "global" ? "" : layers.above[layers.above.length - 1].label}
          onClose={() => setEditing(null)}
          onSave={async (r) => {
            const next = { ...ownRules };
            if (fieldCount(r) === 0) delete next[editing.name];
            else next[editing.name] = r;
            await saveLayer(next);
            setEditing(null);
          }}
        />
      )}
    </Section>
  );
}

// RuleDialog：一条规则在这一层的设置。全局默认每个字段都填（空着表示内置默认）；覆盖层空着表示跟随上一层。
function RuleDialog({ def, scope, own, inherited, from, onClose, onSave }: { def: AlertRuleDef; scope: Scope; own: AlertRule | undefined; inherited: AlertRule; from: string; onClose: () => void; onSave: (r: AlertRule) => Promise<void> }) {
  const [value, setValue] = useState<AlertRule>(own ? create(AlertRuleSchema, own) : create(AlertRuleSchema));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const fields = def.fields.filter((f): f is FieldKey => f in fieldProp);
  const enabled = value.enabled ?? inherited.enabled ?? true;
  const set = (p: keyof AlertRule, v: number | boolean | undefined) => setValue(create(AlertRuleSchema, { ...value, [p]: v }));
  return (
    <Dialog open onClose={onClose} title={ruleName(def.name)} description={scope === "global" ? undefined : m.inherited(from)} className="max-w-[560px]">
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            await onSave(value);
          } catch (err) {
            setError(errorMessage(err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <Check checked={enabled} onChange={(v) => set("enabled", scope !== "global" && v === (inherited.enabled ?? true) ? undefined : v)}>
          {m.enabled}
        </Check>
        {fields.length === 0 ? (
          <p className="text-muted-foreground">{m.noFields}</p>
        ) : (
          <FormRow>
            {fields.map((f) => {
              const p = fieldProp[f];
              const cur = value[p] as number | undefined;
              const base = inherited[p] as number | undefined;
              const unit = unitText(def, f);
              return (
                <Field key={f} label={unit ? m.withUnit(m.fields[f], unit) : m.fields[f]} htmlFor={`rule-${f}`} help={(m.fieldHelp as Record<string, string>)[f]}>
                  <Input
                    id={`rule-${f}`}
                    type="number"
                    step="any"
                    min={0}
                    inputMode="decimal"
                    disabled={!enabled}
                    placeholder={base !== undefined ? m.inheritedValue(String(base)) : ""}
                    value={cur ?? ""}
                    onChange={(e) => set(p, e.target.value === "" ? undefined : Number(e.target.value))}
                  />
                </Field>
              );
            })}
          </FormRow>
        )}
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

// ---- 通用设置：通知语言、抖动保护 ----

function GeneralSection({ policy, builtin }: { policy: AlertPolicy; builtin: AlertPolicy }) {
  const queryClient = useQueryClient();
  const [lang, setLang] = useState(policy.language || "zh-CN");
  const [win, setWin] = useState(policy.flapWindowMinutes || 0);
  const [count, setCount] = useState(policy.flapCount || 0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const dirty = lang !== (policy.language || "zh-CN") || win !== (policy.flapWindowMinutes || 0) || count !== (policy.flapCount || 0);
  return (
    <Section title={m.general}>
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            await alertClient.updateAlertPolicy({ policy: { ...policy, language: lang, flapWindowMinutes: win, flapCount: count }, baseRevision: policy.revision });
            toast.success(m.saved);
          } catch (err) {
            setError(err instanceof ConnectError && err.code === Code.Aborted ? m.conflict : errorMessage(err));
          } finally {
            setBusy(false);
            await queryClient.invalidateQueries({ queryKey: ["alert-rules"] });
          }
        }}
      >
        <div className="grid gap-4 md:grid-cols-3">
          <Field label={m.language} htmlFor="alert-lang">
            <Select id="alert-lang" value={lang} onChange={(e) => setLang(e.target.value)}>
              {locales.map((l) => (
                <option key={l.value} value={l.value}>
                  {l.label}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={m.flapWindow} htmlFor="alert-flap-window" help={m.flapHelp}>
            <Input id="alert-flap-window" type="number" min={0} inputMode="numeric" placeholder={String(builtin.flapWindowMinutes)} value={win || ""} onChange={(e) => setWin(Number(e.target.value) || 0)} />
          </Field>
          <Field label={m.flapCount} htmlFor="alert-flap-count">
            <Input id="alert-flap-count" type="number" min={0} inputMode="numeric" placeholder={String(builtin.flapCount)} value={count || ""} onChange={(e) => setCount(Number(e.target.value) || 0)} />
          </Field>
        </div>
        {error && <Alert>{error}</Alert>}
        <div>
          <Button type="submit" variant="outline" disabled={busy || !dirty}>
            {busy && <Spinner />}
            {t.common.actions.save}
          </Button>
        </div>
      </form>
    </Section>
  );
}

// ---- 事件通知 ----

const noticeText = (n: AlertNoticeDef) => (locale === "en" ? n.en : n.zh);

function NoticesSection({ data }: { data: GetAlertRulesResponse }) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState("");
  const off = data.policy?.notifications ?? {};
  const isOn = (k: string) => off[k] !== false;
  const offCount = data.notices.filter((n) => !isOn(n.key)).length;
  const categories = [...new Set(data.notices.map((n) => n.category))];
  const n = m.notices;

  const toggle = async (def: AlertNoticeDef, enabled: boolean) => {
    if (!enabled) {
      const body = def.risky ? `${n.disableBody}\n\n${n.disableRisky}` : n.disableBody;
      if (!(await confirmAction(`${n.disableTitle(noticeText(def))}\n\n${body}`, { action: n.disable, danger: true }))) return;
    }
    setBusy(def.key);
    try {
      // 关掉要高权限确认：面板要求时弹框，验证后重试一次。版本号每次重新读，免得别人刚改过。
      await withElevation(async () => {
        const cur = await alertClient.getAlertRules({});
        return alertClient.setAlertNotification({ key: def.key, enabled, baseRevision: cur.policy?.revision ?? 0n });
      }, n.elevate);
      toast.success(enabled ? n.turnedOn : n.turnedOff);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy("");
      await queryClient.invalidateQueries({ queryKey: ["alert-rules"] });
    }
  };

  return (
    <Section title={n.title} help={n.help} description={offCount > 0 ? n.offCount(offCount) : n.allOn}>
      {categories.map((c) => (
        <div key={c} className="grid gap-2">
          <h4 className="font-medium">{n.categories[c] ?? c}</h4>
          <SettingRows>
            {data.notices
              .filter((x) => x.category === c)
              .map((x) => {
                const editable = x.security ? data.canEditSecurityNotices : data.canEditNotices;
                const tags = [x.recovers ? n.recovers : n.once];
                if (x.security && !data.canEditSecurityNotices) tags.push(n.adminOnly);
                return (
                  <SettingSwitch
                    key={x.key}
                    label={noticeText(x)}
                    description={
                      <>
                        {tags.join(" · ")}
                        {x.risky && <span className="text-warning-text"> · {n.risky}</span>}
                      </>
                    }
                    on={isOn(x.key)}
                    busy={busy === x.key}
                    unavailable={editable ? undefined : n.adminOnly}
                    onChange={(v) => toggle(x, v)}
                  />
                );
              })}
          </SettingRows>
        </div>
      ))}
    </Section>
  );
}

// AlertRulesLink：节点、项目、容器页上原来的规则编辑器换成这一行：本层覆盖了几项，点开跳到 设置 → 告警 并选好对象。
// 看不到规则（GetAlertRules 拒绝）时只显示说明，没有按钮。
export function AlertRulesLink({ scope, nodeId = "", projectId = "", container = "" }: { scope: "node" | "project" | "container"; nodeId?: string; projectId?: string; container?: string }) {
  const { can } = useRole();
  const allowed = scope === "node" ? can("node.manage") : can("project.alerts") || (scope === "project" && can("project.manage"));
  const rules = useQuery({ queryKey: ["alert-rules"], queryFn: () => alertClient.getAlertRules({}), enabled: allowed });
  const o = rules.data?.overrides.find((x) => x.scope === scope && x.nodeId === nodeId && x.projectId === projectId && x.container === container);
  const n = o ? Object.values(o.rules).reduce((s, r) => s + fieldCount(r), 0) : 0;
  return (
    <SettingRow label={m.rulesTitle} help={m.moved} description={allowed && rules.data ? (n > 0 ? m.summary.some(n) : m.summary.none) : undefined}>
      {allowed && (
        <ButtonLink variant="outline" readOnlyOk to="/settings" search={{ tab: "alerts", scope, node: nodeId || undefined, project: projectId || undefined, container: container || undefined }}>
          {m.openSettings}
        </ButtonLink>
      )}
    </SettingRow>
  );
}
