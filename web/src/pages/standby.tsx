import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { timeAgo } from "@/lib/utils";
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
      !confirm(
        "将本面板提升为主面板？\n\n" +
          "· 主面板仍在运行时不要提升，否则可能出现两份不一致的修改。\n" +
          "· 提升后节点只接受本面板的修改，其他备用面板改为从本面板同步。\n" +
          "· 原主面板恢复后会自动降为备用面板，它最后几秒内未同步的修改将丢失。",
      )
    )
      return;
    setBusy(true);
    try {
      const res = await settingsClient.promoteStandby({});
      alert(`已提升为主面板（任期 ${res.term}），面板正在重新启动，页面将自动刷新。`);
      setTimeout(() => window.location.reload(), 3000);
    } catch (err) {
      alert(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Button readOnlyOk size={size} variant="outline" onClick={promote} disabled={busy}>
      {busy && <Spinner />}
      提升为主面板
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
      <Section title="备用面板" description="本面板是备用面板，实时同步主面板的数据。" actions={<PromoteButton />}>
        <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-2 text-[13px]">
          <dt className="text-muted-foreground">主面板</dt>
          <dd className="font-mono">{d.primaryAddress}</dd>
          <dt className="text-muted-foreground">连接</dt>
          <dd>{d.connected ? <Status tone="success">已连接</Status> : <Status tone="warning">未连接</Status>}</dd>
          <dt className="text-muted-foreground">最近同步</dt>
          <dd title={toDate(d.lastSync)?.toLocaleString()}>{d.lastSync ? timeAgo(toDate(d.lastSync)) : "尚未同步"}</dd>
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
    if (!confirm(`移除备用面板 ${name}？它将无法再同步本面板的数据，备用面板上的数据目录需要手动删除。`)) return;
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
      title="备用面板"
      description="备用面板实时同步本面板的数据，主面板故障时可提升为主面板。"
      help="备用面板经面板端口连接本面板，与本面板共用 CA 和主密钥。平时只读，所有修改在主面板上进行。"
      actions={
        <Button size="sm" variant="outline" onClick={create} disabled={busy}>
          {busy && <Spinner />}
          添加备用面板
        </Button>
      }
    >
      {error && <Alert className="mx-5 mb-3">{error}</Alert>}
      {d.standbys.length === 0 ? (
        <EmptyRow>暂无备用面板</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>名称</Th>
              <Th>地址</Th>
              <Th>状态</Th>
              <Th>最近同步</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {d.standbys.map((s) => (
              <tr key={s.id}>
                <Td className="font-medium">{s.name}</Td>
                <Td className="font-mono text-xs text-muted-foreground">{s.address}</Td>
                <Td>{s.connected ? <Status tone="success">已连接</Status> : <Status>未连接</Status>}</Td>
                <Td className="text-[13px] text-muted-foreground" title={toDate(s.lastSync)?.toLocaleString()}>
                  {s.lastSync ? timeAgo(toDate(s.lastSync)) : "—"}
                </Td>
                <Td className="text-right">
                  <Button size="xs" variant="ghost" onClick={() => remove(s.id, s.name)}>
                    移除
                  </Button>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      <Dialog open={code !== null} onClose={() => setCode(null)} title="添加备用面板" description="在新机器上用配对码启动备用面板。">
        {code && (
          <div className="grid gap-4">
            <Field label="启动命令" hint="数据目录须为空；配对码 1 小时内有效，只能使用一次">
              <CodeBlock copy>{`nagi server --standby ${code.code}`}</CodeBlock>
            </Field>
            <Notice>备用面板启动后自动连接本面板并开始同步，出现在列表中。</Notice>
          </div>
        )}
      </Dialog>
    </Section>
  );
}
