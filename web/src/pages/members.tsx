// 成员：面板的账号和角色。只有管理员能看到。
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { toast } from "sonner";
import { confirmAction } from "@/components/confirm";
import { Alert, Button, Dialog, DialogActions, EmptyRow, Field, Input, List, QueryError, Row, RowMenu, RowsSkeleton, Section, Select, Spinner } from "@/components/ui";
import type { PanelUser } from "@/gen/nagi/v1/api_pb";
import { errorMessage, settingsClient, toDate } from "@/lib/api";
import { formatDate, formatDateTime } from "@/lib/utils";
import { locale, t } from "@/i18n";

const roles = ["admin", "operator", "viewer"] as const;

// shortDate：今天 08:12、昨天、10/28，不是今年的写全年份。
function shortDate(d: Date | undefined) {
  if (!d) return "—";
  const now = new Date();
  const days = Math.round((new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime() - new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()) / 86400000);
  if (days === 0) return `${t.members.today} ${d.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit", hour12: false })}`;
  if (days === 1) return t.members.yesterday;
  if (d.getFullYear() === now.getFullYear()) return d.toLocaleDateString(locale, { month: "2-digit", day: "2-digit" });
  return formatDate(d) ?? "—";
}

const roleOptions = roles.map((r) => (
  <option key={r} value={r}>
    {t.members.roles[r]}
  </option>
));

function AddMember({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
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
      <Field label={t.members.password} htmlFor="member-password" required>
        <Input id="member-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoComplete="new-password" />
      </Field>
      <Field label={t.members.role} htmlFor="member-role" hint={t.members.roleHints[role as (typeof roles)[number]]}>
        <Select id="member-role" value={role} onChange={(e) => setRole(e.target.value)}>
          {roleOptions}
        </Select>
      </Field>
      {add.error && <Alert>{errorMessage(add.error)}</Alert>}
      <DialogActions>
        <Button variant="outline" onClick={onCancel}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={add.isPending}>
          {add.isPending && <Spinner />}
          {t.members.add}
        </Button>
      </DialogActions>
    </form>
  );
}

function ResetPassword({ user, onDone, onCancel }: { user: PanelUser; onDone: () => void; onCancel: () => void }) {
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
      <DialogActions>
        <Button variant="outline" onClick={onCancel}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={reset.isPending}>
          {reset.isPending && <Spinner />}
          {t.members.resetPassword}
        </Button>
      </DialogActions>
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
      title={<span title={t.members.help}>{t.members.title}</span>}
      actions={
        <Button variant="outline" onClick={() => setAdding(true)}>
          <Plus />
          {t.members.add}
        </Button>
      }
    >
      {error && <Alert>{error}</Alert>}
      {users.isPending ? (
        <RowsSkeleton />
      ) : !users.data ? (
        <QueryError error={errorMessage(users.error)} onRetry={() => users.refetch()} />
      ) : users.data.users.length === 0 ? (
        <EmptyRow>{t.members.empty}</EmptyRow>
      ) : (
        <List>
          {users.data.users.map((u) => (
            <Row
              key={String(u.id)}
              title={
                <>
                  <span className="truncate">{u.username}</span>
                  {u.isSelf && <span className="shrink-0 text-muted-foreground">{t.members.you}</span>}
                </>
              }
              sub={<span title={formatDateTime(toDate(u.createdAt))}>{t.members.createdOn(shortDate(toDate(u.createdAt)))}</span>}
              cols={[{ content: u.totpEnabled ? t.members.totpOnShort : t.members.totpOffShort }]}
              actionsWidth="w-40"
              actions={
                <span className="flex items-center gap-1">
                  <Select
                    aria-label={t.members.role}
                    className="h-8 w-[104px]"
                    value={u.role}
                    onChange={async (e) => {
                      const role = e.target.value;
                      if (await confirmAction(t.members.roleConfirm(u.username, t.members.roles[role as (typeof roles)[number]]), { action: t.members.changeRole }))
                        call(() => settingsClient.updateUser({ id: u.id, role }), t.common.feedback.saved);
                    }}
                  >
                    {roleOptions}
                  </Select>
                  <RowMenu
                    items={[
                      { label: t.members.resetPassword, onSelect: () => setResetting(u) },
                      ...(u.totpEnabled && !u.isSelf
                        ? [
                            {
                              label: t.members.clearTotp,
                              onSelect: async () => {
                                if (await confirmAction(t.members.clearTotpConfirm(u.username), { action: t.members.clearTotp, danger: true }))
                                  call(() => settingsClient.updateUser({ id: u.id, clearTotp: true }), t.common.feedback.done);
                              },
                            },
                          ]
                        : []),
                      ...(!u.isSelf
                        ? [
                            {
                              label: t.common.actions.delete,
                              danger: true,
                              onSelect: async () => {
                                if (await confirmAction(t.members.deleteConfirm(u.username), { action: t.common.actions.delete, danger: true }))
                                  call(() => settingsClient.deleteUser({ id: u.id }), t.common.feedback.done);
                              },
                            },
                          ]
                        : []),
                    ]}
                  />
                </span>
              }
            />
          ))}
        </List>
      )}
      <Dialog open={adding} onClose={() => setAdding(false)} title={t.members.add}>
        <AddMember
          onCancel={() => setAdding(false)}
          onDone={() => {
            setAdding(false);
            refresh();
          }}
        />
      </Dialog>
      <Dialog open={!!resetting} onClose={() => setResetting(null)} title={t.members.resetPassword} description={resetting?.username}>
        {resetting && <ResetPassword user={resetting} onCancel={() => setResetting(null)} onDone={() => setResetting(null)} />}
      </Dialog>
    </Section>
  );
}

// 默认显示几条登录设备，其余点开。
const sessionsShown = 3;

// 账号安全里的「登录设备」：当前用户已登录的设备。
export function SessionList() {
  const queryClient = useQueryClient();
  const sessions = useQuery({ queryKey: ["sessions"], queryFn: () => settingsClient.listSessions({}) });
  const [error, setError] = useState("");
  const [all, setAll] = useState(false);
  const [revoking, setRevoking] = useState("");
  if (sessions.isPending) return <RowsSkeleton />;
  if (!sessions.data) return <QueryError error={errorMessage(sessions.error)} onRetry={() => sessions.refetch()} />;
  // 当前设备排最前，收起时也看得到。
  const list = [...sessions.data.sessions.filter((s) => s.current), ...sessions.data.sessions.filter((s) => !s.current)];
  const shown = all ? list : list.slice(0, sessionsShown);
  return (
    <>
      {error && <Alert>{error}</Alert>}
      <List>
        {shown.map((s) => (
          <Row
            key={s.id}
            title={
              <>
                <span className="truncate">{s.ip || t.members.unknownAddress}</span>
                {s.current && <span className="shrink-0 text-muted-foreground">{t.members.thisDevice}</span>}
              </>
            }
            sub={<span title={formatDateTime(toDate(s.createdAt))}>{t.members.signedInAt(shortDate(toDate(s.createdAt)))}</span>}
            actions={
              s.current ? null : (
                <Button
                  size="row"
                  variant="ghost"
                  disabled={revoking !== ""}
                  onClick={async () => {
                    setError("");
                    setRevoking(s.id);
                    try {
                      await settingsClient.revokeSession({ id: s.id });
                      toast.success(t.common.feedback.done);
                      await queryClient.invalidateQueries({ queryKey: ["sessions"] });
                    } catch (err) {
                      setError(errorMessage(err));
                    } finally {
                      setRevoking("");
                    }
                  }}
                >
                  {revoking === s.id && <Spinner />}
                  {t.members.signOutDevice}
                </Button>
              )
            }
          />
        ))}
      </List>
      {!all && list.length > sessionsShown && (
        <div>
          <Button readOnlyOk variant="ghost" className="-ml-3" onClick={() => setAll(true)}>
            {t.members.showAllSessions(list.length)}
          </Button>
        </div>
      )}
    </>
  );
}
