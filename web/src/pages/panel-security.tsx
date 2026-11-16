// 设置 → 安全（只有管理员）：会话的空闲超时、有效期和网络绑定（见 docs/panel.md#会话）。
// 以后的终端录制、日志外送、哈希链状态也放在这一页。
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Alert, Check, QueryError, RowsSkeleton, Section, Select, SettingRow, SettingRows } from "@/components/ui";
import type { SessionPolicy } from "@/gen/nagi/v1/api_pb";
import { errorMessage, settingsClient } from "@/lib/api";
import { t } from "@/i18n";

const idleOptions = [30, 60, 240, 480, 1440];
const maxOptions = [24, 72, 168];

export function PanelSecurityCard() {
  return <SessionSection />;
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
  // 设置里存的值不在选项里时（例如以后改过选项）也显示出来。
  const idle = idleOptions.includes(p.idleMinutes) ? idleOptions : [...idleOptions, p.idleMinutes].sort((a, b) => a - b);
  const maxes = maxOptions.includes(p.maxHours) ? maxOptions : [...maxOptions, p.maxHours].sort((a, b) => a - b);
  return (
    <Section title={m.title} description={m.description} help={m.help}>
      {save.error && <Alert>{errorMessage(save.error)}</Alert>}
      <SettingRows>
        <SettingRow label={m.idle} description={m.idleDescription}>
          <Select aria-label={m.idle} className="w-36" value={String(p.idleMinutes)} onChange={(e) => save.mutate({ idleMinutes: Number(e.target.value) })}>
            {idle.map((v) => (
              <option key={v} value={String(v)}>
                {m.minutes(v)}
              </option>
            ))}
          </Select>
        </SettingRow>
        <SettingRow label={m.maxAge} description={m.maxAgeDescription}>
          <Select aria-label={m.maxAge} className="w-36" value={String(p.maxHours)} onChange={(e) => save.mutate({ maxHours: Number(e.target.value) })}>
            {maxes.map((v) => (
              <option key={v} value={String(v)}>
                {m.hours(v)}
              </option>
            ))}
          </Select>
        </SettingRow>
        <SettingRow label={m.bind} description={m.bindDescription} help={m.bindHelp}>
          <Select aria-label={m.bind} className="w-40" value={p.bind} onChange={(e) => save.mutate({ bind: e.target.value })}>
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
      </SettingRows>
    </Section>
  );
}
