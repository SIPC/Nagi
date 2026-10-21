import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { Alert, Button, CodeBlock, EmptyRow, Section, Table, Td, Th } from "@/components/ui";

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

  const list = invites.data?.invites ?? [];

  return (
    <Section
      title="接收移交的节点"
      description="别的面板要把它的节点交给本面板时，在那边粘贴这里生成的接入码。添加新机器请用节点页的「添加节点」。"
      help="接入码 1 小时内有效，可供多个节点使用。通过接入码加入的节点需在节点页批准后才能使用，以防接入码泄露后被他人利用。"
      actions={
        <Button size="sm" onClick={create}>
          生成接入码
        </Button>
      }
      flush
    >
      {(error || code) && (
        <div className="grid gap-3 px-5 pb-4">
          {error && <Alert>{error}</Alert>}
          {code && (
            <>
              <p className="text-[13px] text-muted-foreground">
                在原面板移交节点时粘贴此接入码，或在节点上执行 <span className="font-mono">nagi panel add &lt;接入码&gt;</span>。
              </p>
              <CodeBlock copy>{code}</CodeBlock>
            </>
          )}
        </div>
      )}
      {list.length > 0 ? (
        <Table>
          <thead>
            <tr>
              <Th>接入码 ID</Th>
              <Th>使用次数</Th>
              <Th>到期时间</Th>
              <Th className="w-px" />
            </tr>
          </thead>
          <tbody>
            {list.map((i) => (
              <tr key={i.id}>
                <Td className="font-mono text-xs">{i.id}</Td>
                <Td className="text-[13px]">{i.uses} 次</Td>
                <Td className="text-[13px] text-muted-foreground">{toDate(i.expiresAt)?.toLocaleTimeString()}</Td>
                <Td>
                  <div className="flex justify-end">
                    <Button
                      size="xs"
                      variant="ghost"
                      onClick={async () => {
                        await settingsClient.revokeInvite({ id: i.id });
                        // 上面显示的接入码可能就是作废的这个：不再显示，有效的看列表。
                        setCode("");
                        invites.refetch();
                      }}
                    >
                      作废
                    </Button>
                  </div>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      ) : (
        <div className="border-t">
          <EmptyRow>暂无有效的接入码</EmptyRow>
        </div>
      )}
    </Section>
  );
}
