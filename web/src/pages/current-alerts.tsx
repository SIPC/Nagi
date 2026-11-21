import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { toast } from "sonner";
import { Button, Dialog, DialogActions, Field, FormBody, List, Notice, QueryError, Row, Status, Textarea } from "@/components/ui";
import { withElevation } from "@/components/elevate";
import { useRole } from "@/components/role";
import type { AlertState } from "@/gen/nagi/v1/alert_state_pb";
import { alertClient, errorMessage, toDate } from "@/lib/api";
import { formatDateTime } from "@/lib/utils";
import { t } from "@/i18n";

export function CurrentAlerts() {
  const { can } = useRole();
  const qc = useQueryClient();
  const query = useQuery({ queryKey: ["alert-states"], queryFn: () => alertClient.listAlertStates({ includeHistory: true }), refetchInterval: 15_000 });
  const [selected, setSelected] = useState<AlertState>();
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [history, setHistory] = useState(false);
  const m = t.logs.alerts;
  const current = query.data?.alerts.filter((a) => a.alert?.status === "firing") ?? [];
  const past = query.data?.alerts.filter((a) => a.alert?.status !== "firing") ?? [];
  const show = (a: AlertState) => { setSelected(a); setNote(a.conclusion); };
  async function handle(action: string) {
    if (!selected?.alert) return;
    setBusy(true);
    try {
      const res = await withElevation(() => alertClient.handleAlert({
        nodeId: selected.nodeId, key: selected.alert!.key, occurrence: selected.alert!.occurrence,
        handlingRevision: selected.handlingRevision, action, conclusion: note,
        until: action === "silence" || action === "maintenance" ? timestampFromDate(new Date(Date.now() + 3_600_000)) : undefined,
      }), m.elevate);
      setSelected(res.alert); setNote(res.alert?.conclusion ?? "");
      await qc.invalidateQueries({ queryKey: ["alert-states"] });
      toast.success(m.saved);
    } catch (err) {
      toast.error(errorMessage(err));
      await qc.invalidateQueries({ queryKey: ["alert-states"] });
      setSelected(undefined);
    } finally { setBusy(false); }
  }
  const live = (a: AlertState) => a.alert?.status === "firing";
  const active = (until: AlertState["silenceUntil"]) => (toDate(until)?.getTime() ?? 0) > Date.now();
  const status = (a: AlertState) => !live(a) ? a.alert?.status === "resolved" ? m.recovered : m.ended
    : a.validity === "stale" ? m.stale : a.validity !== "valid" ? m.unknown : a.acknowledged ? m.acknowledged : m.unacknowledged;
  // An empty current snapshot consumes no page space, including old history.
  if (!current.length && !selected) return query.error ? <QueryError error={query.error} onRetry={() => query.refetch()} /> : null;
  return <section className="mb-6 flex flex-col gap-4" aria-label={m.title}>
    <div className="flex flex-wrap items-center justify-between gap-2">
      <h2 className="text-base font-semibold">{m.title} <span className="ml-2 text-sm font-normal text-muted-foreground">{m.count(current.length)}</span></h2>
      <div className="flex flex-wrap gap-2">
        {!!past.length && <Button variant="ghost" readOnlyOk onClick={() => setHistory(!history)}>{m.history}</Button>}
        <Button variant="ghost" readOnlyOk disabled={query.isFetching} onClick={() => query.refetch()}>{m.refresh}</Button>
      </div>
    </div>
    {query.data?.recentCopy && <Notice tone="warning">{m.recent}</Notice>}
    {query.error && <QueryError error={query.error} onRetry={() => query.refetch()} />}
    <List>{current.map((a) => <Row key={`${a.nodeId}/${a.alert?.occurrence}`} title={a.alert?.title} sub={[a.alert?.project, a.conclusion].filter(Boolean).join(" · ")}
      cols={[{ content: <Status tone="warning">{status(a)}</Status>, phone: true }]}
      actions={<Button variant="ghost" size="row" readOnlyOk onClick={() => show(a)}>{m.handle}</Button>} actionsWidth="w-28" />)}</List>
    {history && <div className="flex flex-col gap-4"><p className="text-sm text-muted-foreground">{m.historyHelp}</p><List>{past.map((a) => <Row key={`${a.nodeId}/${a.alert?.occurrence}`} title={a.alert?.title} sub={status(a)} actions={<Button variant="ghost" size="row" readOnlyOk onClick={() => show(a)}>{m.handle}</Button>} actionsWidth="w-28" />)}</List></div>}
    <Dialog open={!!selected} onClose={() => !busy && setSelected(undefined)} title={selected?.alert?.title ?? m.title} description={m.help}>
      {selected && <FormBody>
        <Status tone={live(selected) ? "warning" : "neutral"}>{status(selected)}</Status>
        <p className="whitespace-pre-wrap break-words text-sm">{selected.alert?.message}</p>
        <p className="text-sm text-muted-foreground">{m.received} {formatDateTime(toDate(selected.receivedAt))}</p>
        {active(selected.silenceUntil) && <p className="text-sm">{m.silenced} · {m.until} {formatDateTime(toDate(selected.silenceUntil))}</p>}
        {active(selected.maintenanceUntil) && <p className="text-sm">{m.maintaining} · {m.until} {formatDateTime(toDate(selected.maintenanceUntil))}</p>}
        <Field label={m.note} htmlFor="alert-note"><Textarea id="alert-note" value={note} maxLength={2000} onChange={(e) => setNote(e.target.value)} disabled={!can("project.alerts") || busy} /></Field>
        {can("project.alerts") && <div className="flex flex-wrap gap-2">
          <Button variant="outline" readOnlyOk disabled={busy || query.data?.recentCopy} onClick={() => handle(selected.acknowledged ? "unacknowledge" : "acknowledge")}>{selected.acknowledged ? m.unacknowledge : m.acknowledge}</Button>
          {live(selected) && <>
            <Button variant="outline" readOnlyOk disabled={busy || query.data?.recentCopy} onClick={() => handle(active(selected.silenceUntil) ? "clear_silence" : "silence")}>{active(selected.silenceUntil) ? m.clearSilence : m.silence}</Button>
            <Button variant="outline" readOnlyOk disabled={busy || query.data?.recentCopy} onClick={() => handle(active(selected.maintenanceUntil) ? "clear_maintenance" : "maintenance")}>{active(selected.maintenanceUntil) ? m.clearMaintenance : m.maintenance}</Button>
            {selected.alert?.source === "panel" && !selected.alert.key.startsWith("offline:") && !selected.alert.key.startsWith("audit-sink:") && <Button variant="outline" readOnlyOk disabled={busy || query.data?.recentCopy} onClick={() => handle("end")}>{t.logs.endAlertEvent}</Button>}
          </>}
        </div>}
      </FormBody>}
      <DialogActions><Button variant="outline" readOnlyOk disabled={busy} onClick={() => setSelected(undefined)}>{t.common.actions.close}</Button>
        {can("project.alerts") && <Button readOnlyOk disabled={busy || query.data?.recentCopy || note === selected?.conclusion} onClick={() => handle("conclusion")}>{m.saveNote}</Button>}
      </DialogActions>
    </Dialog>
  </section>;
}
