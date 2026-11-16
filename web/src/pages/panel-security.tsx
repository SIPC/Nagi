// 设置 → 安全（只有管理员）：终端录制、审计记录的保留时间、日志外送、哈希链（见 docs/audit.md），
// 以及会话的空闲超时、有效期和网络绑定（见 docs/panel.md#会话）。
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Plus } from "lucide-react";
import { toast } from "sonner";
import { Alert, Button, Check, Dialog, DialogActions, EmptyRow, Field, FormBody, Input, List, QueryError, Row, RowMenu, RowsSkeleton, Section, Select, SettingRow, SettingRows, Skeleton, Spinner, Status, Textarea } from "@/components/ui";
import { create } from "@bufbuild/protobuf";
import { AuditSinkHeaderSchema, AuditSinkSchema, type AuditSettings, type AuditSink, type SessionPolicy } from "@/gen/nagi/v1/api_pb";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { isElevationRequired, requireElevation } from "@/components/elevate";
import { confirmAction } from "@/components/confirm";
import { formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";

const idleOptions = [30, 60, 240, 480, 1440];
const maxOptions = [24, 72, 168];
const retentionOptions = [30, 90, 180, 365];
const longRetentionOptions = [90, 180, 365, 730, 1095];

export function PanelSecurityCard() {
  return (
    <>
      <AuditSections />
      <SessionSection />
    </>
  );
}

// withOption：设置里存的值不在选项里时（例如以后改过选项）也显示出来。
function withOption(options: number[], v: number) {
  return options.includes(v) ? options : [...options, v].sort((a, b) => a - b);
}

function AuditSections() {
  const queryClient = useQueryClient();
  const settings = useQuery({ queryKey: ["audit-settings"], queryFn: () => settingsClient.getAuditSettings({}), refetchInterval: 15000 });
  const save = useMutation({
    mutationFn: async (patch: Partial<AuditSettings>) => {
      const cur = settings.data!;
      // 关闭录制要先验证身份（见 docs/audit.md#终端与命令录制）。
      if (patch.recording === false && cur.recording && !(await requireElevation(t.panelSecurity.recording.elevate, { suggestTotp: false }))) return cur;
      return settingsClient.setAuditSettings({ ...cur, ...patch });
    },
    onSuccess: (s) => queryClient.setQueryData(["audit-settings"], s),
  });
  if (settings.isPending) return <RowsSkeleton count={4} />;
  if (!settings.data) return <QueryError error={errorMessage(settings.error)} onRetry={() => settings.refetch()} />;
  const s = settings.data;
  const r = t.panelSecurity.recording;
  const ret = t.panelSecurity.retention;
  // 缩短保留时间会马上删掉超期的记录，先确认。
  const setRetention = async (kind: "normal" | "long", days: number) => {
    const cur = kind === "normal" ? s.retentionDays : s.longRetentionDays;
    if (days < cur && !(await confirmAction(ret.shortenConfirm(kind === "normal" ? ret.normal : ret.long, days), { action: ret.shorten, danger: true }))) return;
    save.mutate(kind === "normal" ? { retentionDays: days, longRetentionDays: Math.max(s.longRetentionDays, days) } : { longRetentionDays: days });
  };
  return (
    <>
      {save.error && <Alert>{errorMessage(save.error)}</Alert>}
      <Section title={r.title} description={r.description} help={r.help}>
        <SettingRows>
          <SettingRow label={r.label}>
            <Check checked={s.recording} disabled={save.isPending} onChange={(v) => save.mutate({ recording: v })}>
              <span className="sr-only">{r.label}</span>
            </Check>
          </SettingRow>
        </SettingRows>
      </Section>
      <Section title={ret.title} description={ret.description} help={ret.help}>
        <SettingRows>
          <SettingRow label={ret.normal}>
            <Select aria-label={ret.normal} className="w-36" value={String(s.retentionDays)} onChange={(e) => setRetention("normal", Number(e.target.value))}>
              {withOption(retentionOptions, s.retentionDays).map((v) => (
                <option key={v} value={String(v)}>
                  {ret.days(v)}
                </option>
              ))}
            </Select>
          </SettingRow>
          <SettingRow label={ret.long} description={ret.longDescription}>
            <Select aria-label={ret.long} className="w-36" value={String(s.longRetentionDays)} onChange={(e) => setRetention("long", Number(e.target.value))}>
              {withOption(longRetentionOptions, s.longRetentionDays)
                .filter((v) => v >= s.retentionDays)
                .map((v) => (
                  <option key={v} value={String(v)}>
                    {ret.days(v)}
                  </option>
                ))}
            </Select>
          </SettingRow>
        </SettingRows>
      </Section>
      <SinkSection settings={s} onSave={(sinks) => save.mutateAsync({ sinks })} />
      <ChainSection settings={s} />
    </>
  );
}

function sinkSummary(k: AuditSink) {
  if (k.type === "http") return k.url;
  if (k.type === "syslog") return k.address;
  return `${k.endpoint} / ${k.bucket}${k.prefix ? `/${k.prefix}` : ""}`;
}

function SinkSection({ settings, onSave }: { settings: AuditSettings; onSave: (sinks: AuditSink[]) => Promise<unknown> }) {
  const m = t.panelSecurity.sinks;
  const [editing, setEditing] = useState<AuditSink | "new">();
  const remove = async (k: AuditSink) => {
    if (!(await confirmAction(m.removeConfirm(k.name), { action: m.removeAction, danger: true }))) return;
    try {
      await onSave(settings.sinks.filter((x) => x.id !== k.id));
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };
  const test = async (k: AuditSink) => {
    try {
      await settingsClient.testAuditSink(k);
      toast.success(m.tested);
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };
  const addButton = (
    <Button variant="outline" onClick={() => setEditing("new")}>
      <Plus />
      {m.add}
    </Button>
  );
  return (
    <Section title={m.title} description={m.description} help={m.help} actions={settings.sinks.length > 0 ? addButton : undefined}>
      {settings.sinks.length === 0 ? (
        <EmptyRow action={addButton}>{m.empty}</EmptyRow>
      ) : (
        <List>
          {settings.sinks.map((k) => (
            <Row
              key={k.id}
              title={<span className="truncate">{k.name}</span>}
              sub={
                <>
                  <span className="block wrap-anywhere">{[m.types[k.type as keyof typeof m.types] ?? k.type, sinkSummary(k)].join(" · ")}</span>
                  {k.failing && k.lastError && <span className="block text-destructive wrap-anywhere">{k.lastError}</span>}
                </>
              }
              cols={[
                {
                  phone: true,
                  width: "w-40",
                  content: k.failing ? (
                    <Status tone="danger">{m.failing}</Status>
                  ) : k.lastSuccess ? (
                    <span title={formatDateTime(toDate(k.lastSuccess))}>
                      <Status tone="success">{m.sentAgo(timeAgo(toDate(k.lastSuccess)))}</Status>
                    </span>
                  ) : (
                    <Status tone="neutral">{m.waiting}</Status>
                  ),
                },
              ]}
              actions={
                <RowMenu
                  items={[
                    { label: t.common.actions.edit, onSelect: () => setEditing(k) },
                    { label: m.test, onSelect: () => test(k) },
                    { label: m.remove, onSelect: () => remove(k), danger: true },
                  ]}
                />
              }
            />
          ))}
        </List>
      )}
      {editing && (
        <SinkDialog
          sink={editing === "new" ? undefined : editing}
          onClose={() => setEditing(undefined)}
          onSave={async (k) => {
            const list = editing === "new" ? [...settings.sinks, k] : settings.sinks.map((x) => (x.id === k.id ? k : x));
            await onSave(list);
            setEditing(undefined);
          }}
        />
      )}
    </Section>
  );
}

function SinkDialog({ sink, onClose, onSave }: { sink?: AuditSink; onClose: () => void; onSave: (k: AuditSink) => Promise<void> }) {
  const m = t.panelSecurity.sinks;
  const [k, setK] = useState<AuditSink>(() => (sink ? { ...sink, secretKey: "", headers: [] } : create(AuditSinkSchema, { type: "http" })));
  const [headerText, setHeaderText] = useState(() => (sink?.headers ?? []).map((h) => `${h.name}: `).join("\n"));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<"" | "save" | "test">("");
  const set = (patch: Partial<AuditSink>) => setK((cur) => ({ ...cur, ...patch }) as AuditSink);
  // 请求头一行一个「名称: 值」；值留空表示沿用已保存的。
  const build = () => {
    const headers = headerText
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean)
      .map((l) => {
        const i = l.indexOf(":");
        return { name: (i < 0 ? l : l.slice(0, i)).trim(), value: i < 0 ? "" : l.slice(i + 1).trim() };
      });
    return create(AuditSinkSchema, { ...k, headers: headers.map((h) => create(AuditSinkHeaderSchema, h)) });
  };
  const run = async (what: "save" | "test") => {
    setBusy(what);
    setError("");
    try {
      if (what === "test") {
        await settingsClient.testAuditSink(build());
        toast.success(m.tested);
      } else {
        await onSave(build());
      }
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy("");
    }
  };
  return (
    <Dialog open onClose={onClose} title={sink ? m.edit : m.add} className="max-w-[520px]">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          run("save");
        }}
      >
        <FormBody className="gap-4">
          <Field label={m.name} htmlFor="sink-name">
            <Input id="sink-name" value={k.name} onChange={(e) => set({ name: e.target.value })} required autoFocus />
          </Field>
          <Field label={m.type} htmlFor="sink-type">
            <Select id="sink-type" value={k.type} onChange={(e) => set({ type: e.target.value })}>
              <option value="http">{m.types.http}</option>
              <option value="syslog">{m.types.syslog}</option>
              <option value="s3">{m.types.s3}</option>
            </Select>
          </Field>
          {k.type === "http" && (
            <>
              <Field label={m.url} htmlFor="sink-url" hint={m.urlHint}>
                <Input id="sink-url" value={k.url} onChange={(e) => set({ url: e.target.value })} placeholder="https://logs.example.com/ingest" required />
              </Field>
              <Field label={m.headers} htmlFor="sink-headers" hint={sink ? m.headersKeep : m.headersHint}>
                <Textarea id="sink-headers" rows={3} className="font-mono text-[13px]" value={headerText} onChange={(e) => setHeaderText(e.target.value)} placeholder={m.headersPlaceholder} />
              </Field>
            </>
          )}
          {k.type === "syslog" && (
            <>
              <Field label={m.address} htmlFor="sink-address" hint={m.addressHint}>
                <Input id="sink-address" value={k.address} onChange={(e) => set({ address: e.target.value })} placeholder="logs.example.com:6514" required />
              </Field>
              <Field label={m.ca} htmlFor="sink-ca" hint={m.caHint}>
                <Textarea id="sink-ca" rows={3} className="font-mono text-[13px]" value={k.ca} onChange={(e) => set({ ca: e.target.value })} placeholder="-----BEGIN CERTIFICATE-----" />
              </Field>
            </>
          )}
          {k.type === "s3" && (
            <>
              <Field label={m.endpoint} htmlFor="sink-endpoint">
                <Input id="sink-endpoint" value={k.endpoint} onChange={(e) => set({ endpoint: e.target.value })} placeholder="https://s3.example.com" required />
              </Field>
              <div className="grid grid-cols-2 gap-4 max-md:grid-cols-1">
                <Field label={m.bucket} htmlFor="sink-bucket">
                  <Input id="sink-bucket" value={k.bucket} onChange={(e) => set({ bucket: e.target.value })} required />
                </Field>
                <Field label={m.region} htmlFor="sink-region">
                  <Input id="sink-region" value={k.region} onChange={(e) => set({ region: e.target.value })} placeholder="us-east-1" />
                </Field>
              </div>
              <Field label={m.prefix} htmlFor="sink-prefix">
                <Input id="sink-prefix" value={k.prefix} onChange={(e) => set({ prefix: e.target.value })} placeholder="nagi/audit" />
              </Field>
              <div className="grid grid-cols-2 gap-4 max-md:grid-cols-1">
                <Field label="Access Key" htmlFor="sink-ak">
                  <Input id="sink-ak" value={k.accessKey} onChange={(e) => set({ accessKey: e.target.value })} required autoComplete="off" />
                </Field>
                <Field label="Secret Key" htmlFor="sink-sk" hint={sink?.hasSecret ? m.secretKeep : undefined}>
                  <Input id="sink-sk" type="password" value={k.secretKey} onChange={(e) => set({ secretKey: e.target.value })} required={!sink?.hasSecret} autoComplete="new-password" />
                </Field>
              </div>
              <Check checked={k.recordings} onChange={(v) => set({ recordings: v })} help={m.recordingsHelp}>
                {m.recordings}
              </Check>
            </>
          )}
          {error && <Alert>{error}</Alert>}
        </FormBody>
        <DialogActions>
          <Button type="button" variant="ghost" className="mr-auto" disabled={busy !== ""} onClick={() => run("test")}>
            {busy === "test" && <Spinner />}
            {m.test}
          </Button>
          <Button type="button" variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy !== ""}>
            {busy === "save" && <Spinner />}
            {t.common.actions.save}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

function ChainSection({ settings }: { settings: AuditSettings }) {
  const queryClient = useQueryClient();
  const m = t.panelSecurity.chain;
  const verify = useMutation({
    mutationFn: () => settingsClient.verifyAudit({}),
    onSuccess: (chain) => queryClient.setQueryData(["audit-settings"], { ...settings, chain }),
  });
  const c = settings.chain;
  return (
    <Section title={m.title} description={m.description} help={m.help}>
      {verify.error && <Alert>{errorMessage(verify.error)}</Alert>}
      <SettingRows>
        <SettingRow
          label={m.verify}
          description={
            !c?.verifiedAt ? (
              m.never
            ) : c.ok ? (
              <span title={formatDateTime(toDate(c.verifiedAt))}>{m.ok(c.records.toLocaleString(), timeAgo(toDate(c.verifiedAt)))}</span>
            ) : (
              <span className="text-destructive">{c.reason}</span>
            )
          }
        >
          <Button variant="outline" disabled={verify.isPending} onClick={() => verify.mutate()}>
            {verify.isPending && <Spinner />}
            {m.verifyNow}
          </Button>
        </SettingRow>
        <SettingRow
          label={m.anchor}
          description={
            c?.anchorAt ? (
              <span title={c.anchorHash}>{m.anchorAt(String(c.anchorId), timeAgo(toDate(c.anchorAt)), c.anchorHash.slice(0, 12))}</span>
            ) : (
              m.noAnchor
            )
          }
        />
      </SettingRows>
    </Section>
  );
}

function SessionSection() {
  const queryClient = useQueryClient();
  const policy = useQuery({ queryKey: ["session-policy"], queryFn: () => settingsClient.getSessionPolicy({}) });
  const save = useMutation({
    mutationFn: (p: Partial<SessionPolicy>) => settingsClient.setSessionPolicy({ ...policy.data!, ...p }),
    onSuccess: (p) => queryClient.setQueryData(["session-policy"], p),
  });
  const m = t.panelSecurity.session;
  if (policy.isPending) return <RowsSkeleton count={4} />;
  if (!policy.data) return <QueryError error={errorMessage(policy.error)} onRetry={() => policy.refetch()} />;
  const p = policy.data;
  return (
    <Section title={m.title} description={m.description} help={m.help}>
      {save.error && <Alert>{errorMessage(save.error)}</Alert>}
      <SettingRows>
        <SettingRow label={m.idle} description={m.idleDescription}>
          <Select aria-label={m.idle} className="w-36" value={String(p.idleMinutes)} onChange={(e) => save.mutate({ idleMinutes: Number(e.target.value) })}>
            {withOption(idleOptions, p.idleMinutes).map((v) => (
              <option key={v} value={String(v)}>
                {m.minutes(v)}
              </option>
            ))}
          </Select>
        </SettingRow>
        <SettingRow label={m.maxAge} description={m.maxAgeDescription}>
          <Select aria-label={m.maxAge} className="w-36" value={String(p.maxHours)} onChange={(e) => save.mutate({ maxHours: Number(e.target.value) })}>
            {withOption(maxOptions, p.maxHours).map((v) => (
              <option key={v} value={String(v)}>
                {m.hours(v)}
              </option>
            ))}
          </Select>
        </SettingRow>
        <SettingRow label={m.bind} description={m.bindDescription} help={m.bindHelp}>
          <Select aria-label={m.bind} className="w-36" value={p.bind} onChange={(e) => save.mutate({ bind: e.target.value })}>
            <option value="subnet">{m.bindSubnet}</option>
            <option value="ip">{m.bindIP}</option>
            <option value="off">{m.bindOff}</option>
          </Select>
        </SettingRow>
        <SettingRow label={m.bindAll} description={m.bindAllDescription}>
          <Check checked={p.bindAll} disabled={p.bind === "off"} onChange={(v) => save.mutate({ bindAll: v })}>
            <span className="sr-only">{m.bindAll}</span>
          </Check>
        </SettingRow>
        <AdminNetworksRow />
      </SettingRows>
    </Section>
  );
}

// AdminNetworksRow 是管理员登录地址（见 docs/panel.md#管理员登录地址）：一行概括，在对话框里修改。
function AdminNetworksRow() {
  const m = t.panelSecurity.session;
  const nets = useQuery({ queryKey: ["admin-networks"], queryFn: () => settingsClient.getAdminNetworks({}) });
  const [editing, setEditing] = useState(false);
  const list = nets.data?.networks ?? [];
  // 加载中不能写「不限制」：可能其实有限制。失败时红字写原因，按钮换成重试。
  const description = nets.isPending ? <Skeleton className="my-1 h-3 w-40" /> : nets.isError ? <span className="text-destructive">{errorMessage(nets.error)}</span> : <span className="break-all">{list.length ? t.common.list(list) : m.adminNetworksOff}</span>;
  return (
    <SettingRow label={m.adminNetworks} help={m.adminNetworksHelp} description={description}>
      {nets.isError ? (
        <Button variant="outline" readOnlyOk onClick={() => nets.refetch()}>
          {t.common.actions.retry}
        </Button>
      ) : (
        <Button variant="outline" disabled={!nets.data} onClick={() => setEditing(true)}>
          {m.edit}
        </Button>
      )}
      {editing && nets.data && <AdminNetworksDialog networks={list} currentIp={nets.data.currentIp} onClose={() => setEditing(false)} />}
    </SettingRow>
  );
}

function AdminNetworksDialog({ networks, currentIp, onClose }: { networks: string[]; currentIp: string; onClose: () => void }) {
  const m = t.panelSecurity.session;
  const queryClient = useQueryClient();
  const [text, setText] = useState(networks.join("\n"));
  const save = useMutation({
    mutationFn: async () => {
      const req = { networks: text.split("\n").map((v) => v.trim()).filter(Boolean) };
      try {
        return await settingsClient.setAdminNetworks(req);
      } catch (err) {
        if (!isElevationRequired(err) || !(await requireElevation(m.elevate))) throw err;
        return settingsClient.setAdminNetworks(req);
      }
    },
    onSuccess: (res) => {
      queryClient.setQueryData(["admin-networks"], res);
      toast.success(m.saved);
      onClose();
    },
  });
  return (
    <Dialog open onClose={onClose} title={m.dialogTitle} description={m.adminNetworksHelp}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <Field label={m.field} htmlFor="admin-networks" hint={m.fieldHint(currentIp)}>
          <Textarea id="admin-networks" rows={4} className="font-mono text-[13px]" autoFocus value={text} onChange={(e) => setText(e.target.value)} placeholder={currentIp} />
        </Field>
        {save.error && <Alert>{errorMessage(save.error)}</Alert>}
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={save.isPending}>
            {save.isPending && <Spinner />}
            {t.common.actions.save}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
