import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Button, List, Row, Status } from "@/components/ui";
import { useRole } from "@/components/role";
import type { AlertBaselineState } from "@/gen/nagi/v1/alert_state_pb";
import { alertClient, toDate } from "@/lib/api";
import { formatDateTime } from "@/lib/utils";
import { t } from "@/i18n";

// AlertBaselines：日志页「当前告警」下面的基线建立进度（见 docs/monitoring.md#基线）。
// 突增类规则要先积累一段历史才能判断，这里按规则和对象列出要多久、积累了多少、预计什么时候可用。
// 全部可用时收成一行，可以展开；没有用到基线的规则时整块不显示。面板按告警权限和访问范围过滤（ListAlertBaselines）。
const m = t.alerts.policy.baselines;

const hours = (s: number) => m.hours(Math.round((s / 3600) * 10) / 10);

function state(b: AlertBaselineState): "building" | "ready" | "stalled" | "unsupported" {
  if (b.unsupported) return "unsupported";
  const s = b.baseline?.status;
  return s === "ready" || s === "stalled" ? s : "building";
}

const tone = { building: "info", ready: "success", stalled: "warning", unsupported: "neutral" } as const;

export function AlertBaselines() {
  const { can } = useRole();
  const allowed = can("panel.audit") || can("project.alerts");
  const query = useQuery({ queryKey: ["alert-baselines"], queryFn: () => alertClient.listAlertBaselines({}), enabled: allowed, refetchInterval: 60_000 });
  const [open, setOpen] = useState(false);
  const rows = query.data?.baselines ?? [];
  if (!allowed || rows.length === 0) return null;
  const pending = rows.filter((b) => state(b) !== "ready").length;
  const expanded = pending > 0 || open;
  return (
    <section className="mb-6 flex flex-col gap-4" aria-label={m.title}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">
          {m.title} <span className="ml-2 text-sm font-normal text-muted-foreground">{pending > 0 ? m.pending(pending) : m.allReady(rows.length)}</span>
        </h2>
        {pending === 0 && (
          <Button variant="ghost" readOnlyOk aria-expanded={open} onClick={() => setOpen(!open)}>
            {open ? m.hide : m.show}
          </Button>
        )}
      </div>
      {expanded && (
        <>
          <p className="text-sm text-muted-foreground">{m.help}</p>
          <List>
            {rows.map((b) => (
              <BaselineRow key={`${b.nodeId}/${b.baseline?.rule}/${b.baseline?.projectId}/${b.baseline?.subject}`} b={b} />
            ))}
          </List>
        </>
      )}
    </section>
  );
}

function BaselineRow({ b }: { b: AlertBaselineState }) {
  const s = state(b);
  const need = b.baseline?.requiredSeconds ?? 0;
  const have = Math.min(b.baseline?.collectedSeconds ?? 0, need);
  const pct = need > 0 ? (have / need) * 100 : 0;
  const rule = t.alerts.policy.rule[b.baseline?.rule ?? ""] ?? b.baseline?.rule;
  const object = [b.project || b.node, b.baseline?.subject].filter(Boolean).join(" / ");
  // 预计可用：还差的时长加到现在；数据中断时不估计。
  const eta = s === "building" ? m.eta(formatDateTime(new Date(Date.now() + (need - have) * 1000)) ?? "") : undefined;
  const sub =
    s === "unsupported" ? (
      m.unsupportedHelp
    ) : (
      <span className="flex flex-col gap-1">
        <span className="block h-1 max-w-80 overflow-hidden rounded-full bg-muted" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)} aria-label={`${rule} ${object}`}>
          <span className={s === "stalled" ? "block h-full rounded-full bg-warning" : "block h-full rounded-full bg-primary"} style={{ width: `${pct}%` }} />
        </span>
        <span>
          {[m.progress(hours(have), hours(need)), eta, s === "stalled" ? m.stalledHelp + (b.baseline?.lastSample ? ` ${m.lastSample(formatDateTime(toDate(b.baseline.lastSample)) ?? "")}` : "") : undefined].filter(Boolean).join(" · ")}
        </span>
      </span>
    );
  return (
    <Row
      title={
        <>
          <span className="truncate">{rule}</span>
          <span className="truncate text-muted-foreground">{object}</span>
        </>
      }
      sub={sub}
      cols={[{ content: <Status tone={tone[s]}>{m[s]}</Status>, phone: true }]}
    />
  );
}
