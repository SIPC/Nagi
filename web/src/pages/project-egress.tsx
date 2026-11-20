// 项目设置里的「出站限制」（见 docs/agent.md#出站限制）：容器能不能访问节点本机、私有网段和云元数据地址。
// 放行等于扩大权限，只有管理员能编辑；其他角色只看到当前的设置，没有按钮。
// 设置行写当前状态，编辑放进对话框；放宽（拦截改成仅记录或关闭、放行一个地址）前先确认。
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { toast } from "sonner";
import { Alert, Button, Dialog, DialogActions, EmptyRow, Field, List, Notice, QueryError, Row, RowsSkeleton, Section, Segmented, SettingRow, SettingRows, Spinner, Textarea } from "@/components/ui";
import type { Project } from "@/gen/nagi/v1/common_pb";
import { errorMessage, projectClient, settingsClient, toDate } from "@/lib/api";
import { confirmAction } from "@/components/confirm";
import { HelpParts } from "@/components/help-parts";
import { useRole } from "@/components/role";
import { timeAgo } from "@/lib/utils";
import { t } from "@/i18n";

const week = 7 * 24 * 3600 * 1000;

type Mode = "block" | "log" | "off";
// 从严到宽，用来判断这次是不是放宽。
const strictness: Record<Mode, number> = { block: 2, log: 1, off: 0 };

// 有效的模式：没有设置的（升级前创建的项目）按仅记录。
function modeOf(project: Project): Mode {
  const m = project.settings?.egress?.mode;
  return m === "block" || m === "off" ? m : "log";
}

// useSaveEgress：接口每次提交完整的设置，其余设置照原样带上。
function useSaveEgress(project: Project) {
  const queryClient = useQueryClient();
  return async (next: { mode?: Mode; allow?: string[] }) => {
    const s = project.settings;
    await projectClient.updateProjectSettings({
      projectId: project.id,
      baseRevision: project.revision,
      name: project.name,
      networkName: project.networkName,
      settings: {
        internalPorts: s?.internalPorts,
        snapshotIntervalMinutes: s?.snapshotIntervalMinutes,
        snapshotLimit: s?.snapshotLimit,
        egress: { mode: next.mode ?? modeOf(project), allow: next.allow ?? s?.egress?.allow ?? [] },
      },
    });
    toast.success(t.alerts.savedAndPushed);
    await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
  };
}

export function EgressSection({ project }: { project: Project }) {
  // 出站限制归「网络和域名」。
  const isAdmin = useRole().can("project.network");
  const m = t.projectSettings.egress;
  const mode = modeOf(project);
  const allow = project.settings?.egress?.allow ?? [];
  const save = useSaveEgress(project);
  const [editing, setEditing] = useState<"mode" | "allow" | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");

  // 最近 7 天被记录的目标：节点发出的出站告警（键 egress:<项目>/<容器>/<目标>）。
  // 不按「egress:」搜索：非管理员的搜索不匹配参数，会一条也查不到。取本项目的告警，在下面按键筛。
  const hits = useQuery({
    queryKey: ["egress-hits", project.id],
    queryFn: () =>
      settingsClient.listAuditEvents({
        filter: { projectId: project.id, categories: ["alert"], from: timestampFromDate(new Date(Date.now() - week)) },
        limit: 200,
      }),
    refetchInterval: 30000,
  });
  const targets = new Map<string, { target: string; container: string; time?: Date }>();
  for (const e of hits.data?.events ?? []) {
    try {
      const key: string = JSON.parse(e.params).key ?? "";
      if (!key.startsWith("egress:")) continue;
      const parts = key.slice("egress:".length).split("/");
      if (parts.length < 3) continue;
      const target = parts.slice(2).join("/");
      if (!targets.has(target)) targets.set(target, { target, container: parts[1], time: toDate(e.time) });
    } catch {
      // 不是出站告警。
    }
  }

  // 放行一个被记录的目标：扩大了容器能访问的范围，先确认。
  const allowTarget = async (target: string) => {
    if (!(await confirmAction(m.allowConfirm(target), { action: m.allowTarget }))) return;
    setBusy(target);
    setError("");
    try {
      await save({ allow: [...allow, target] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy("");
    }
  };

  return (
    <Section title={m.title} help={<HelpParts parts={[m.description, m.help]} />}>
      {mode === "log" && (
        <Notice tone="warning" title={m.logOnly}>
          {m.logOnlyHint}
        </Notice>
      )}
      {error && <Alert>{error}</Alert>}
      <SettingRows>
        <SettingRow label={m.mode} description={m.modeStatus[mode]}>
          {isAdmin && (
            <Button variant="outline" onClick={() => setEditing("mode")}>
              {t.common.actions.edit}
            </Button>
          )}
        </SettingRow>
        <SettingRow label={m.allow} description={allow.length > 0 ? <span className="font-mono">{t.common.list(allow)}</span> : m.noAllow}>
          {isAdmin && (
            <Button variant="outline" onClick={() => setEditing("allow")}>
              {t.common.actions.edit}
            </Button>
          )}
        </SettingRow>
      </SettingRows>
      {editing === "mode" && <ModeDialog project={project} onClose={() => setEditing(null)} />}
      {editing === "allow" && <AllowDialog project={project} onClose={() => setEditing(null)} />}
      {/* 最近记录是另一组内容：小标题和上面的设置隔 24。 */}
      <div className="mt-2 grid gap-2">
        <h3 className="text-sm font-medium">{m.recent}</h3>
        {hits.isPending ? (
          <RowsSkeleton />
        ) : hits.isError ? (
          <QueryError error={hits.error} onRetry={() => hits.refetch()} />
        ) : targets.size === 0 ? (
          <EmptyRow>{m.noRecent}</EmptyRow>
        ) : (
          <List>
            {[...targets.values()].map((x) => (
              <Row
                key={x.target}
                title={<span className="font-mono text-[13px]">{x.target}</span>}
                sub={[x.container, x.time && timeAgo(x.time)].filter(Boolean).join(" · ")}
                actions={
                  isAdmin && !allow.includes(x.target) ? (
                    <Button size="row" variant="ghost" disabled={!!busy} onClick={() => allowTarget(x.target)}>
                      {busy === x.target && <Spinner />}
                      {m.allowTarget}
                    </Button>
                  ) : null
                }
              />
            ))}
          </List>
        )}
      </div>
    </Section>
  );
}

// ModeDialog：选择模式。比现在宽松（拦截改成仅记录、关闭）时先确认，写清后果。
function ModeDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const m = t.projectSettings.egress;
  const current = modeOf(project);
  const save = useSaveEgress(project);
  const [mode, setMode] = useState<Mode>(current);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    if (mode === current) return onClose();
    if (strictness[mode] < strictness[current] && !(await confirmAction(m.loosenConfirm[mode as "log" | "off"], { action: m.loosenAction[mode as "log" | "off"], danger: true }))) return;
    setBusy(true);
    setError("");
    try {
      await save({ mode });
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} title={m.editMode}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <Field label={m.mode} hint={m.modeHints[mode]}>
          <Segmented
            label={m.mode}
            value={mode}
            onChange={setMode}
            items={[
              { value: "block", label: m.modes.block },
              { value: "log", label: m.modes.log },
              { value: "off", label: m.modes.off },
            ]}
          />
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

// AllowDialog：允许访问的内网地址，一行一个。
function AllowDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const m = t.projectSettings.egress;
  const allow = project.settings?.egress?.allow ?? [];
  const save = useSaveEgress(project);
  const [text, setText] = useState(allow.join("\n"));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const lines = () =>
    text
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean);
  const submit = async () => {
    if (lines().join("\n") === allow.join("\n")) return onClose();
    setBusy(true);
    setError("");
    try {
      await save({ allow: lines() });
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} title={m.editAllow}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <Field label={m.allow} htmlFor={`egress-allow-${project.id}`} hint={m.allowHint}>
          <Textarea id={`egress-allow-${project.id}`} rows={4} className="font-mono text-[13px] max-md:text-base" value={text} onChange={(e) => setText(e.target.value)} placeholder="192.168.1.20:5432" spellCheck={false} />
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
