import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, nodeClient } from "@/lib/api";
import {
  Alert,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Spinner,
  Textarea,
} from "@/components/ui";

// ---- 换面板与多面板（临时测试界面）----

// HandOverCard：把勾选的节点交给另一个面板（移交，或共同管理）。
export function HandOverCard({
  nodes,
}: {
  nodes: { id: string; name: string; online: boolean }[];
}) {
  const queryClient = useQueryClient();
  const [code, setCode] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [move, setMove] = useState(true);
  const [busy, setBusy] = useState(false);
  const [results, setResults] = useState<string[]>([]);

  const run = async () => {
    if (move && !confirm("移交后本面板会忘记这些节点。确定？")) return;
    setBusy(true);
    const out: string[] = [];
    for (const id of selected) {
      const name = nodes.find((n) => n.id === id)?.name ?? id;
      try {
        await nodeClient.addNodePanel({
          nodeId: id,
          invite: code,
          removeSelf: move,
        });
        out.push(`✅ ${name}`);
      } catch (err) {
        out.push(`❌ ${name}：${errorMessage(err)}`);
      }
      setResults([...out]);
    }
    setBusy(false);
    setSelected([]);
    queryClient.invalidateQueries({ queryKey: ["nodes"] });
  };

  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>交给另一个面板</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        <Textarea
          className="font-mono text-xs"
          rows={2}
          placeholder="粘贴新面板生成的接入码 nagi-invite:..."
          value={code}
          onChange={(e) => setCode(e.target.value)}
        />
        <div className="flex flex-wrap gap-3">
          {nodes.map((n) => (
            <label
              key={n.id}
              className={n.online ? "" : "text-muted-foreground"}
            >
              <input
                type="checkbox"
                className="mr-1"
                disabled={!n.online}
                checked={selected.includes(n.id)}
                onChange={(e) =>
                  setSelected(
                    e.target.checked
                      ? [...selected, n.id]
                      : selected.filter((x) => x !== n.id),
                  )
                }
              />
              {n.name}
              {!n.online && "（离线）"}
            </label>
          ))}
        </div>
        <div className="flex gap-4">
          <label>
            <input
              type="radio"
              className="mr-1"
              checked={move}
              onChange={() => setMove(true)}
            />
            移交（本面板随后移除自己、忘记这些节点）
          </label>
          <label>
            <input
              type="radio"
              className="mr-1"
              checked={!move}
              onChange={() => setMove(false)}
            />
            共同管理（两个面板都保留）
          </label>
        </div>
        <div>
          <Button
            size="sm"
            disabled={busy || !code || selected.length === 0}
            onClick={run}
          >
            {busy ? "处理中…" : "开始"}
          </Button>
        </div>
        {results.length > 0 && (
          <pre className="text-xs">{results.join("\n")}</pre>
        )}
      </CardContent>
    </Card>
  );
}

// NodePanels：节点信任的面板列表。
export function NodePanels({ nodeId }: { nodeId: string }) {
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ["node-panels", nodeId],
    queryFn: () => nodeClient.listNodePanels({ nodeId }),
  });
  const [error, setError] = useState("");
  if (q.isPending) return <Spinner />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  return (
    <div className="grid gap-1 py-2 text-xs">
      {error && <Alert>{error}</Alert>}
      {q.data.panels.map((p) => (
        <div key={p.fingerprint} className="flex items-center gap-3">
          <span className="w-44 font-mono">{p.address}</span>
          <span className="w-56 truncate font-mono text-muted-foreground">
            {p.fingerprint}
          </span>
          {p.isSelf && <Badge>本面板</Badge>}
          {p.connected ? (
            <Badge tone="success">已连接</Badge>
          ) : (
            <Badge>未连接</Badge>
          )}
          <Button
            size="sm"
            variant="ghost"
            onClick={async () => {
              const msg = p.isSelf
                ? "从节点上移除本面板？本面板会忘记这个节点，节点由其他面板继续管理。"
                : `从节点上移除面板 ${p.address}？`;
              if (!confirm(msg)) return;
              setError("");
              try {
                await nodeClient.removeNodePanel({
                  nodeId,
                  fingerprint: p.fingerprint,
                });
                queryClient.invalidateQueries({
                  queryKey: p.isSelf ? ["nodes"] : ["node-panels", nodeId],
                });
              } catch (err) {
                setError(errorMessage(err));
              }
            }}
          >
            移除
          </Button>
        </div>
      ))}
    </div>
  );
}
