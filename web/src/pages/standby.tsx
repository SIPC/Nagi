import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { Alert, Button, CodeBlock, Dialog, EmptyRow, Field, Notice, Section, Spinner, Status, Table, Td, Th } from "@/components/ui";

// 「备用面板」区块（见 docs/standby.md）：主面板上列出备用面板、生成配对码；备用面板上显示自己的同步状态。

export function useStandbys() {
  return useQuery({ queryKey: ["standbys"], queryFn: () => settingsClient.getStandbys({}), refetchInterval: 5000 });
}

// PromoteButton：把本面板（备用面板）提升为主面板。提升后面板按主面板重新启动，页面几秒后刷新。
export function PromoteButton({ size = "sm" }: { size?: "sm" | "xs" }) {
  const [busy, setBusy] = useState(false);
  const promote = async () => {
    if (
      !confirm(t.standby.promote.confirm)
    )
      return;
    setBusy(true);
    try {
      const res = await settingsClient.promoteStandby({});
      alert(t.standby.promote.done(res.term));
      setTimeout(() => window.location.reload(), 3000);
    } catch (err) {
      alert(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Button readOnlyOk size={size} variant="outline" onClick={promote} disabled={busy}>
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

  if (q.isPending) return null;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  const d = q.data;

  if (d.isStandby) {
    return (
      <Section title={t.standby.status.title} description={t.standby.status.description} actions={<PromoteButton />}>
        <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 text-[13px]">
          <dt className="text-muted-foreground">{t.standby.status.primary}</dt>
          <dd className="font-mono">{d.primaryAddress}</dd>
          <dt className="text-muted-foreground">{t.standby.status.connection}</dt>
          <dd>{d.connected ? <Status tone="success">{t.standby.connected}</Status> : <Status tone="warning">{t.standby.disconnected}</Status>}</dd>
          <dt className="text-muted-foreground">{t.standby.lastSync}</dt>
          <dd title={formatDateTime(toDate(d.lastSync))}>{d.lastSync ? timeAgo(toDate(d.lastSync)) : t.standby.status.neverSynced}</dd>
        </dl>
        {d.lastError && <Alert className="mt-4">{d.lastError}</Alert>}
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
    if (!confirm(t.standby.removeConfirm(name))) return;
    setError("");
    try {
      await settingsClient.removeStandby({ id });
      queryClient.invalidateQueries({ queryKey: ["standbys"] });
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Section
      flush
      title={t.standby.list.title}
      help={t.standby.list.help}
      actions={
        <Button size="sm" variant="outline" onClick={create} disabled={busy}>
          {busy && <Spinner />}
          {t.standby.list.add}
        </Button>
      }
    >
      {error && <Alert className="mb-3">{error}</Alert>}
      {d.standbys.length === 0 ? (
        <EmptyRow>{t.standby.list.empty}</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>{t.standby.list.name}</Th>
              <Th>{t.standby.list.address}</Th>
              <Th>{t.standby.list.status}</Th>
              <Th>{t.standby.lastSync}</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {d.standbys.map((s) => (
              <tr key={s.id}>
                <Td className="font-medium">{s.name}</Td>
                <Td className="font-mono text-xs text-muted-foreground">{s.address}</Td>
                <Td>{s.connected ? <Status tone="success">{t.standby.connected}</Status> : <Status>{t.standby.disconnected}</Status>}</Td>
                <Td className="text-[13px] text-muted-foreground" title={formatDateTime(toDate(s.lastSync))}>
                  {s.lastSync ? timeAgo(toDate(s.lastSync)) : "—"}
                </Td>
                <Td className="text-right">
                  <Button size="xs" variant="ghost" onClick={() => remove(s.id, s.name)}>
                    {t.common.actions.remove}
                  </Button>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      <Dialog open={code !== null} onClose={() => setCode(null)} title={t.standby.dialog.title} description={t.standby.dialog.description}>
        {code && (
          <div className="grid gap-4">
            <Field label={t.standby.dialog.command} hint={t.standby.dialog.commandHint}>
              <CodeBlock copy>{`nagi server --standby ${code.code}`}</CodeBlock>
            </Field>
            <Notice>{t.standby.dialog.notice}</Notice>
          </div>
        )}
      </Dialog>
    </Section>
  );
}
