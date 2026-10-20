import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { Alert, Button, Card, CardContent, CardHeader, CardTitle } from "@/components/ui";

// InviteCard：本面板生成接入码，其他面板用它把节点交过来。
export function InviteCard() {
  const invites = useQuery({
    queryKey: ["invites"],
    queryFn: () => settingsClient.listInvites({}),
  });
  const [code, setCode] = useState("");
  const [error, setError] = useState("");

  const create = async () => {
    setError("");
    try {
      const res = await settingsClient.createInvite({});
      setCode(res.invite);
      invites.refetch();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>接入码（把其他面板的节点接过来）</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {error && <Alert>{error}</Alert>}
        <div>
          <Button size="sm" onClick={create}>
            生成接入码
          </Button>
        </div>
        {code && (
          <div className="grid gap-1">
            <span className="text-muted-foreground">
              复制到旧面板的「交给另一个面板」，或在节点上执行 nagi panel add
              &lt;接入码&gt;。1 小时内可以给多个节点用。用它加入的节点要在节点页「批准」后才能使用（接入码泄露的话别人也能加入）：
            </span>
            <code className="break-all rounded bg-muted p-3 font-mono text-xs">
              {code}
            </code>
          </div>
        )}
        {invites.data && invites.data.invites.length > 0 && (
          <div className="grid gap-1">
            <span className="text-muted-foreground">有效的接入码：</span>
            {invites.data.invites.map((i) => (
              <div key={i.id} className="flex items-center gap-3 text-xs">
                <span className="font-mono">{i.id}</span>
                <span>已用 {i.uses} 次</span>
                <span className="text-muted-foreground">
                  到期 {toDate(i.expiresAt)?.toLocaleTimeString()}
                </span>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={async () => {
                    await settingsClient.revokeInvite({ id: i.id });
                    invites.refetch();
                  }}
                >
                  作废
                </Button>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

