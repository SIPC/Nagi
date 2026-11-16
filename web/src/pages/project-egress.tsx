// 项目设置里的「出站限制」（见 docs/agent.md#出站限制）：容器能不能访问节点本机、私有网段和云元数据地址。
// 放行等于扩大权限，只有管理员能改；其他角色只能看。
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { toast } from "sonner";
import { Alert, Button, EmptyRow, Field, FormActions, FormBody, List, Notice, Row, Section, Select, SettingRow, SettingRows, Spinner, Textarea } from "@/components/ui";
import type { Project } from "@/gen/nagi/v1/common_pb";
import { errorMessage, projectClient, settingsClient, toDate } from "@/lib/api";
import { useRole } from "@/components/role";
import { timeAgo } from "@/lib/utils";
import { t } from "@/i18n";

const week = 7 * 24 * 3600 * 1000;

// 有效的模式：没有设置的（升级前创建的项目）按仅记录。
function modeOf(project: Project) {
  const m = project.settings?.egress?.mode;
  return m === "block" || m === "off" ? m : "log";
}

export function EgressSection({ project }: { project: Project }) {
  const queryClient = useQueryClient();
  const { isAdmin } = useRole();
  const m = t.projectSettings.egress;
  const mode = modeOf(project);
  const allow = project.settings?.egress?.allow ?? [];
  const [text, setText] = useState(allow.join("\n"));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  // 最近 7 天被记录的目标：节点发出的出站告警（键 egress:<项目>/<容器>/<目标>）。
  const hits = useQuery({
    queryKey: ["egress-hits", project.id],
    queryFn: () =>
      settingsClient.listAuditEvents({
        filter: { projectId: project.id, categories: ["alert"], search: "egress:", from: timestampFromDate(new Date(Date.now() - week)) },
        limit: 200,
      }),
    refetchInterval: 30000,
  });
  const targets = new Map<string, { target: string; container: string; time?: Date }>();
  for (const e of hits.data?.events ?? []) {
    try {
      const key: string = JSON.parse(e.params).key ?? "";
      const [, rest] = key.split(":", 2);
      const parts = key.slice("egress:".length).split("/");
      if (!rest || parts.length < 3) continue;
      const target = parts.slice(2).join("/");
      if (!targets.has(target)) targets.set(target, { target, container: parts[1], time: toDate(e.time) });
    } catch {
      // 不是出站告警。
    }
  }

  const save = async (next: { mode?: string; allow?: string[] }) => {
    setBusy(true);
    setError("");
    try {
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
          egress: { mode: next.mode ?? mode, allow: next.allow ?? allow },
        },
      });
      toast.success(t.alerts.savedAndPushed);
      await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  const lines = () =>
    text
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean);

  return (
    <Section title={m.title} description={m.description} help={m.help}>
      {mode === "log" && (
        <Notice tone="warning" title={m.logOnly} actions={isAdmin && <Button variant="outline" disabled={busy} onClick={() => save({ mode: "block" })}>{m.switchToBlock}</Button>}>
          {m.logOnlyHint}
        </Notice>
      )}
      {error && <Alert>{error}</Alert>}
      <SettingRows>
        <SettingRow label={m.mode} description={m.modeHints[mode]}>
          <Select aria-label={m.mode} className="w-32" value={mode} disabled={!isAdmin || busy} onChange={(e) => save({ mode: e.target.value })}>
            <option value="block">{m.modes.block}</option>
            <option value="log">{m.modes.log}</option>
            <option value="off">{m.modes.off}</option>
          </Select>
        </SettingRow>
      </SettingRows>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          save({ allow: lines() });
        }}
      >
        <FormBody>
          <Field label={m.allow} htmlFor={`egress-allow-${project.id}`} hint={m.allowHint}>
            <Textarea
              id={`egress-allow-${project.id}`}
              rows={3}
              className="font-mono text-[13px]"
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder="192.168.1.20:5432"
              disabled={!isAdmin}
            />
          </Field>
        </FormBody>
        {isAdmin && (
          <FormActions>
            <Button type="submit" variant="outline" disabled={busy || lines().join("\n") === allow.join("\n")}>
              {busy && <Spinner />}
              {t.common.actions.save}
            </Button>
          </FormActions>
        )}
      </form>
      <div className="grid gap-2">
        <h3 className="text-sm font-medium">{m.recent}</h3>
        {targets.size === 0 ? (
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
                    <Button size="row" variant="ghost" disabled={busy} onClick={() => save({ allow: [...allow, x.target] })}>
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
