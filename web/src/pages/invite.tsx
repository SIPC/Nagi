import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { Alert, Button, CodeBlock, EmptyRow, Section, Table, Td, Th } from "@/components/ui";
import { formatTime } from "@/lib/utils";
import { t } from "@/i18n";

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
      title={t.invite.title}
      description={t.invite.description}
      help={t.invite.help}
      actions={
        <Button size="sm" onClick={create}>
          {t.invite.generate}
        </Button>
      }
      flush
    >
      {(error || code) && (
        <div className="grid gap-3 pb-4">
          {error && <Alert>{error}</Alert>}
          {code && (
            <>
              <p className="text-[13px] text-muted-foreground">
                {t.invite.pasteHint(<span className="font-mono">{t.invite.command}</span>)}
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
              <Th>{t.invite.id}</Th>
              <Th>{t.invite.uses}</Th>
              <Th>{t.invite.expires}</Th>
              <Th className="w-px" />
            </tr>
          </thead>
          <tbody>
            {list.map((i) => (
              <tr key={i.id}>
                <Td className="font-mono text-xs">{i.id}</Td>
                <Td className="text-[13px]">{t.invite.usesCount(i.uses)}</Td>
                <Td className="text-[13px] text-muted-foreground">{formatTime(toDate(i.expiresAt))}</Td>
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
                      {t.invite.revoke}
                    </Button>
                  </div>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      ) : (
        <EmptyRow>{t.invite.empty}</EmptyRow>
      )}
    </Section>
  );
}
