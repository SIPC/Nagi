import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { confirmAction } from "@/components/confirm";
import { Alert, Button, CopyButton, EmptyRow, List, QueryError, Row, RowsSkeleton, Section, Spinner } from "@/components/ui";
import { formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";

// CodeBox：一行命令或接入码加复制按钮，放不下时截断，完整内容在悬停提示里。
export function CodeBox({ text, title }: { text: string; title?: string }) {
  return (
    <div className="flex min-h-10 min-w-0 items-center justify-between gap-2 rounded-md bg-accent pr-2 pl-3">
      <span className="min-w-0 truncate font-mono text-[13px] leading-5" title={title ?? text}>
        {text}
      </span>
      <CopyButton text={text} className="shrink-0" />
    </div>
  );
}

// InviteCard：本面板生成接入码，其他面板用它把节点交过来。
export function InviteCard() {
  const invites = useQuery({
    queryKey: ["invites"],
    queryFn: () => settingsClient.listInvites({}),
  });
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [creating, setCreating] = useState(false);
  const [revoking, setRevoking] = useState("");

  const create = async () => {
    setError("");
    setCreating(true);
    try {
      const res = await settingsClient.createInvite({});
      setCode(res.invite);
      invites.refetch();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setCreating(false);
    }
  };

  const revoke = async (id: string) => {
    if (!(await confirmAction(t.invite.revokeConfirm(id), { action: t.invite.revokeAction, danger: true }))) return;
    setError("");
    setRevoking(id);
    try {
      await settingsClient.revokeInvite({ id });
      // 上面显示的接入码可能就是作废的这个：不再显示，有效的看列表。
      setCode("");
      await invites.refetch();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setRevoking("");
    }
  };

  const generateButton = (
    <Button variant="outline" onClick={create} disabled={creating}>
      {creating && <Spinner />}
      {t.invite.generate}
    </Button>
  );
  return (
    <Section
      title={t.settings.tabs.invite.label}
      help={t.invite.help}
      description={t.invite.description(
        <Link to="/nodes" className="text-foreground underline-offset-4 hover:underline">
          {t.invite.addNode}
        </Link>,
      )}
      actions={generateButton}
    >
      {error && <Alert>{error}</Alert>}
      {code && (
        <div className="grid gap-2">
          <CodeBox text={code} />
          <p className="text-[13px] leading-5 text-muted-foreground">{t.invite.pasteHint(t.invite.command)}</p>
        </div>
      )}
      {invites.isPending ? (
        <RowsSkeleton count={2} />
      ) : !invites.data ? (
        <QueryError error={errorMessage(invites.error)} onRetry={() => invites.refetch()} />
      ) : invites.data.invites.length === 0 ? (
        <EmptyRow action={generateButton}>{t.invite.empty}</EmptyRow>
      ) : (
        <List>
          {invites.data.invites.map((i) => (
            <Row
              key={i.id}
              title={<span className="truncate">{i.id}</span>}
              sub={t.invite.usedCount(i.uses)}
              cols={[{ content: <span title={formatDateTime(toDate(i.expiresAt))}>{t.invite.expiresIn(timeAgo(toDate(i.expiresAt)))}</span>, num: true }]}
              actions={
                <Button size="row" variant="ghost" disabled={revoking !== ""} onClick={() => revoke(i.id)}>
                  {revoking === i.id && <Spinner />}
                  {t.invite.revoke}
                </Button>
              }
            />
          ))}
        </List>
      )}
    </Section>
  );
}
