import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { CodeBox } from "@/pages/invite";
import { Alert, Button, Dialog, DialogActions, EmptyRow, KeyValue, List, Notice, QueryError, Row, RowsSkeleton, Section, Spinner, Status } from "@/components/ui";
import { confirmAction, notify } from "@/components/confirm";

// 「备用面板」区块（见 docs/standby.md）：主面板上列出备用面板、生成配对码；备用面板上显示自己的同步状态。

export function useStandbys() {
  return useQuery({ queryKey: ["standbys"], queryFn: () => settingsClient.getStandbys({}), refetchInterval: 5000 });
}

// PromoteButton：把本面板（备用面板）提升为主面板。提升后面板按主面板重新启动，页面几秒后刷新。
export function PromoteButton({ size = "sm", variant = "outline" }: { size?: "sm" | "xs"; variant?: "outline" | "destructive-outline" }) {
  const [busy, setBusy] = useState(false);
  const promote = async () => {
    if (!(await confirmAction(t.standby.promote.confirm, { action: t.standby.promote.button, danger: true }))) return;
    setBusy(true);
    try {
      const res = await settingsClient.promoteStandby({});
      await notify(t.standby.promote.done(res.term));
      setTimeout(() => window.location.reload(), 3000);
    } catch (err) {
      await notify(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Button readOnlyOk size={size} variant={variant} onClick={promote} disabled={busy}>
      {busy && <Spinner />}
      {t.standby.promote.button}
    </Button>
  );
}

export function StandbyCard() {
  const queryClient = useQueryClient();
  const q = useStandbys();
  const [code, setCode] = useState<{ code: string; expires?: Date } | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState("");

  if (q.isPending)
    return (
      <Section title={t.settings.tabs.standby.label}>
        <RowsSkeleton />
      </Section>
    );
  if (!q.data)
    return (
      <Section title={t.settings.tabs.standby.label}>
        <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />
      </Section>
    );
  const d = q.data;
  // 每 5 秒刷新一次：刷新失败时列表留着，上面提示数据不是最新的。
  const refetchError = q.error && <QueryError error={errorMessage(q.error)} onRetry={() => q.refetch()} />;

  if (d.isStandby) {
    return (
      <Section title={<span title={t.standby.status.description}>{t.standby.status.title}</span>}>
        {refetchError}
        <KeyValue
          items={[
            [t.standby.status.primary, <span className="wrap-anywhere">{d.primaryAddress}</span>],
            [t.standby.status.connection, d.connected ? <Status tone="success">{t.standby.connected}</Status> : <span className="text-warning-text"><Status tone="warning">{t.standby.disconnected}</Status></span>],
            [t.standby.lastSync, <span title={formatDateTime(toDate(d.lastSync))}>{d.lastSync ? timeAgo(toDate(d.lastSync)) : t.standby.status.neverSynced}</span>],
          ]}
        />
        {d.lastError && (
          <Notice tone="danger" title={t.standby.status.syncFailed}>
            {d.lastError}
          </Notice>
        )}
        <div>
          <PromoteButton variant="destructive-outline" />
        </div>
      </Section>
    );
  }

  const create = async () => {
    setBusy(true);
    setError("");
    try {
      const res = await settingsClient.createStandbyCode({});
      setCode({ code: res.code, expires: toDate(res.expiresAt) });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  const remove = async (id: string, name: string) => {
    if (!(await confirmAction(t.standby.removeConfirm(name), { action: t.common.actions.remove, danger: true }))) return;
    setError("");
    setRemoving(id);
    try {
      await settingsClient.removeStandby({ id });
      await queryClient.invalidateQueries({ queryKey: ["standbys"] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setRemoving("");
    }
  };

  return (
    <Section
      title={t.standby.list.title}
      help={t.standby.list.help}
      actions={
        <Button variant="outline" onClick={create} disabled={busy}>
          {busy ? <Spinner /> : <Plus />}
          {t.standby.list.add}
        </Button>
      }
    >
      {refetchError}
      {error && <Alert>{error}</Alert>}
      {d.standbys.length === 0 ? (
        <EmptyRow>{t.standby.list.empty}</EmptyRow>
      ) : (
        <List>
          {d.standbys.map((s) => (
            <Row
              key={s.id}
              title={<span className="truncate">{s.name}</span>}
              sub={
                <span className="wrap-anywhere" title={formatDateTime(toDate(s.lastSync))}>{`${s.address} · ${t.standby.lastSync} ${s.lastSync ? timeAgo(toDate(s.lastSync)) : "—"}`}</span>
              }
              cols={[
                {
                  phone: true,
                  content: s.connected ? (
                    <Status tone="success">{t.standby.connected}</Status>
                  ) : (
                    <span className="text-warning-text" title={t.standby.disconnectedHint}>
                      <Status tone="warning">{t.standby.disconnected}</Status>
                    </span>
                  ),
                },
              ]}
              actions={
                <Button size="row" variant="ghost" disabled={removing !== ""} onClick={() => remove(s.id, s.name)}>
                  {removing === s.id && <Spinner />}
                  {t.common.actions.remove}
                </Button>
              }
            />
          ))}
        </List>
      )}
      <Dialog open={code !== null} onClose={() => setCode(null)} title={t.standby.dialog.title} className="max-w-[520px]">
        {code && (
          <>
            <CodeBox text={`nagi server --standby ${code.code}`} />
            <p className="text-[13px] leading-5 text-muted-foreground">{t.standby.dialog.notice}</p>
            <DialogActions>
              <Button readOnlyOk onClick={() => setCode(null)}>
                {t.standby.dialog.done}
              </Button>
            </DialogActions>
          </>
        )}
      </Dialog>
    </Section>
  );
}
