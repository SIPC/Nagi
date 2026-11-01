// 成员：面板的账号和角色。只有管理员能看到。
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { confirmAction } from "@/components/confirm";
import { Alert, Badge, Button, Dialog, EmptyRow, Field, Input, Loading, Section, Select, Spinner, Table, Td, Th } from "@/components/ui";
import type { PanelUser } from "@/gen/nagi/v1/api_pb";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { formatDate } from "@/lib/utils";
import { t } from "@/i18n";

const roles = ["admin", "operator", "viewer"] as const;

function RoleOptions() {
  return (
    <>
      {roles.map((r) => (
        <option key={r} value={r}>
          {t.members.roles[r]}
        </option>
      ))}
    </>
  );
}

function AddMember({ onDone }: { onDone: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<string>("operator");
  const add = useMutation({
    mutationFn: () => settingsClient.createUser({ username, password, role }),
    onSuccess: () => {
      toast.success(t.members.added(username));
      onDone();
    },
  });
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        add.mutate();
      }}
    >
      <Field label={t.members.username} htmlFor="member-name" required>
        <Input id="member-name" value={username} onChange={(e) => setUsername(e.target.value)} required maxLength={32} autoComplete="off" />
      </Field>
      <Field label={t.members.password} htmlFor="member-password" hint={t.members.passwordHint} required>
        <Input id="member-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoComplete="new-password" />
      </Field>
      <Field label={t.members.role} htmlFor="member-role" hint={t.members.roleHints[role as (typeof roles)[number]]}>
        <Select id="member-role" value={role} onChange={(e) => setRole(e.target.value)}>
          <RoleOptions />
        </Select>
      </Field>
      {add.error && <Alert>{errorMessage(add.error)}</Alert>}
      <div className="flex justify-end">
        <Button type="submit" size="sm" disabled={add.isPending}>
          {add.isPending && <Spinner />}
          {t.members.add}
        </Button>
      </div>
    </form>
  );
}

function ResetPassword({ user, onDone }: { user: PanelUser; onDone: () => void }) {
  const [password, setPassword] = useState("");
  const reset = useMutation({
    mutationFn: () => settingsClient.updateUser({ id: user.id, password }),
    onSuccess: () => {
      toast.success(t.members.passwordReset(user.username));
      onDone();
    },
  });
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        reset.mutate();
      }}
    >
      <Field label={t.members.newPassword} htmlFor="member-new-password" hint={t.members.passwordHint} required>
        <Input id="member-new-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoComplete="new-password" />
      </Field>
      {reset.error && <Alert>{errorMessage(reset.error)}</Alert>}
      <div className="flex justify-end">
        <Button type="submit" size="sm" disabled={reset.isPending}>
          {reset.isPending && <Spinner />}
          {t.members.resetPassword}
        </Button>
      </div>
    </form>
  );
}

export function MembersCard() {
  const queryClient = useQueryClient();
  const users = useQuery({ queryKey: ["users"], queryFn: () => settingsClient.listUsers({}) });
  const [adding, setAdding] = useState(false);
  const [resetting, setResetting] = useState<PanelUser | null>(null);
  const [error, setError] = useState("");
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["users"] });
  const call = async (fn: () => Promise<unknown>, done: string) => {
    setError("");
    try {
      await fn();
      toast.success(done);
    } catch (err) {
      setError(errorMessage(err));
    }
    refresh();
  };

  return (
    <Section
      flush
      title={t.members.title}
      help={t.members.help}
      actions={
        <Button size="sm" variant="outline" onClick={() => setAdding(true)}>
          {t.members.add}
        </Button>
      }
    >
      {error && <Alert className="mb-3">{error}</Alert>}
      {users.isPending ? (
        <Loading />
      ) : users.error ? (
        <Alert>{errorMessage(users.error)}</Alert>
      ) : users.data.users.length === 0 ? (
        <EmptyRow>{t.members.empty}</EmptyRow>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>{t.members.username}</Th>
              <Th>{t.members.role}</Th>
              <Th className="hidden sm:table-cell">{t.members.totp}</Th>
              <Th className="hidden sm:table-cell">{t.members.createdAt}</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {users.data.users.map((u) => (
              <tr key={String(u.id)}>
                <Td className="font-medium">
                  {u.username}
                  {u.isSelf && <Badge className="ml-2">{t.members.you}</Badge>}
                </Td>
                <Td>
                  <Select
                    aria-label={t.members.role}
                    className="h-8 w-28 text-[13px]"
                    value={u.role}
                    onChange={(e) => call(() => settingsClient.updateUser({ id: u.id, role: e.target.value }), t.common.feedback.saved)}
                  >
                    <RoleOptions />
                  </Select>
                </Td>
                <Td className="hidden text-[13px] text-muted-foreground sm:table-cell">{u.totpEnabled ? t.members.totpOn : t.members.totpOff}</Td>
                <Td className="hidden text-[13px] text-muted-foreground sm:table-cell">{formatDate(toDate(u.createdAt))}</Td>
                <Td className="text-right whitespace-nowrap">
                  <Button size="xs" variant="ghost" onClick={() => setResetting(u)}>
                    {t.members.resetPassword}
                  </Button>
                  {u.totpEnabled && !u.isSelf && (
                    <Button
                      size="xs"
                      variant="ghost"
                      onClick={async () => {
                        if (await confirmAction(t.members.clearTotpConfirm(u.username), { action: t.members.clearTotp, danger: true }))
                          call(() => settingsClient.updateUser({ id: u.id, clearTotp: true }), t.common.feedback.done);
                      }}
                    >
                      {t.members.clearTotp}
                    </Button>
                  )}
                  {!u.isSelf && (
                    <Button
                      size="xs"
                      variant="ghost"
                      onClick={async () => {
                        if (await confirmAction(t.members.deleteConfirm(u.username), { action: t.common.actions.delete, danger: true }))
                          call(() => settingsClient.deleteUser({ id: u.id }), t.common.feedback.done);
                      }}
                    >
                      {t.common.actions.delete}
                    </Button>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      <Dialog open={adding} onClose={() => setAdding(false)} title={t.members.add}>
        <AddMember
          onDone={() => {
            setAdding(false);
            refresh();
          }}
        />
      </Dialog>
      <Dialog open={!!resetting} onClose={() => setResetting(null)} title={t.members.resetPassword} description={resetting?.username}>
        {resetting && <ResetPassword user={resetting} onDone={() => setResetting(null)} />}
      </Dialog>
    </Section>
  );
}

// SessionsRow 的内容：当前用户已登录的设备。
export function SessionList() {
  const queryClient = useQueryClient();
  const sessions = useQuery({ queryKey: ["sessions"], queryFn: () => settingsClient.listSessions({}) });
  const [error, setError] = useState("");
  if (sessions.isPending) return <Loading />;
  if (sessions.error) return <Alert>{errorMessage(sessions.error)}</Alert>;
  return (
    <div className="grid gap-2">
      {error && <Alert>{error}</Alert>}
      {sessions.data.sessions.map((s) => (
        <div key={s.id} className="flex items-center justify-between gap-3 text-[13px]">
          <span className="min-w-0">
            <span className="font-mono text-xs">{s.ip || t.members.unknownAddress}</span>
            <span className="ml-2 text-muted-foreground">{t.members.signedInAt(formatDate(toDate(s.createdAt)) ?? "—")}</span>
          </span>
          {s.current ? (
            <Badge>{t.members.thisDevice}</Badge>
          ) : (
            <Button
              size="xs"
              variant="ghost"
              onClick={async () => {
                setError("");
                try {
                  await settingsClient.revokeSession({ id: s.id });
                  toast.success(t.common.feedback.done);
                  queryClient.invalidateQueries({ queryKey: ["sessions"] });
                } catch (err) {
                  setError(errorMessage(err));
                }
              }}
            >
              {t.members.signOutDevice}
            </Button>
          )}
        </div>
      ))}
    </div>
  );
}
