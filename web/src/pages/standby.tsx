import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { capitalize, formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { Alert, Button, Dialog, DialogActions, EmptyRow, HelpTip, List, Notice, QueryError, Row, RowsSkeleton, Section, SettingRow, SettingRows, Spinner, Status, CodeBlock } from "@/components/ui";
import { confirmAction, notify } from "@/components/confirm";

// 「备用面板」区块（见 docs/standby.md）：主面板上列出备用面板、生成配对码；备用面板上显示自己的同步状态。

export function useStandbys() {
  return useQuery({ queryKey: ["standbys"], queryFn: () => settingsClient.getStandbys({}), refetchInterval: 5000 });
}

// PromoteButton：把本面板（备用面板）提升为主面板。提升后面板按主面板重新启动，页面几秒后刷新。
// onError：提升失败的原因，由放按钮的地方就地显示（传空串表示清掉上一次的错误）。
export function PromoteButton({ variant = "outline", onError }: { variant?: "outline" | "destructive-outline"; onError: (message: string) => void }) {
  const [busy, setBusy] = useState(false);
  const promote = async () => {
    if (!(await confirmAction(t.standby.promote.confirm, { action: t.standby.promote.button, danger: true }))) return;
    setBusy(true);
    onError("");
    try {
      await settingsClient.promoteStandby({});
      await notify(t.standby.promote.done);
      setTimeout(() => window.location.reload(), 3000);
    } catch (err) {
      onError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Button readOnlyOk variant={variant} onClick={promote} disabled={busy}>
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

  // 备用面板上：同步状态写成设置行，提升为主面板放在主面板那一行。
  if (d.isStandby) {
    return (
      <Section title={t.settings.tabs.standby.label} help={t.standby.status.help}>
        {refetchError}
        {error && <Alert>{error}</Alert>}
        {d.lastError && (
          <Notice tone="danger" title={t.standby.status.syncFailed}>
            {d.lastError}
          </Notice>
        )}
        <SettingRows>
          <SettingRow label={t.standby.status.primary} description={<span className="wrap-anywhere">{d.primaryAddress}</span>}>
            <PromoteButton variant="destructive-outline" onError={setError} />
          </SettingRow>
          <SettingRow
            label={t.standby.status.connection}
            description={d.connected ? <Status tone="success">{t.standby.connected}</Status> : <Status tone="warning">{t.standby.disconnected}</Status>}
          />
          <SettingRow
            label={t.standby.lastSync}
            description={<span title={formatDateTime(toDate(d.lastSync))}>{d.lastSync ? capitalize(timeAgo(toDate(d.lastSync))) : t.standby.status.neverSynced}</span>}
          />
        </SettingRows>
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
    if (!(await confirmAction(t.standby.removeConfirm(name), { action: t.standby.remove, danger: true }))) return;
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

  const addButton = (
    <Button variant="outline" onClick={create} disabled={busy}>
      {busy ? <Spinner /> : <Plus />}
      {t.standby.list.add}
    </Button>
  );
  return (
    <Section title={t.settings.tabs.standby.label} help={t.standby.list.help} actions={addButton}>
      {refetchError}
      {error && <Alert>{error}</Alert>}
      {d.standbys.length === 0 ? (
        <EmptyRow action={addButton}>{t.standby.list.empty}</EmptyRow>
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
                  // 英文「Not connected」加问号放不进默认的列宽。
                  width: "w-36",
                  content: s.connected ? (
                    <Status tone="success">{t.standby.connected}</Status>
                  ) : (
                    <span className="flex items-center gap-1">
                      <Status tone="warning">{t.standby.disconnected}</Status>
                      <HelpTip>{t.standby.disconnectedHint}</HelpTip>
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
      <Dialog open={code !== null} onClose={() => setCode(null)} title={t.standby.dialog.title} description={t.standby.dialog.description} className="max-w-[520px]">
        {code && (
          <>
            <CodeBlock copy nowrap>{`nagi server --standby ${code.code}`}</CodeBlock>
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
