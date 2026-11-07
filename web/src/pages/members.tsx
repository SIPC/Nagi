// 成员：面板的账号和角色。只有管理员能看到。
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { toast } from "sonner";
import { confirmAction } from "@/components/confirm";
import { avatarSrc, UserAvatar } from "@/components/account";
import { Alert, Button, Check, Collapse, Dialog, DialogActions, EmptyRow, Field, Input, List, QueryError, Row, RowMenu, RowsSkeleton, Section, Segmented, Select, Spinner, Status } from "@/components/ui";
import type { PanelUser } from "@/gen/nagi/v1/api_pb";
import { errorMessage, nodeClient, projectClient, settingsClient, toDate } from "@/lib/api";
import { AccessTokenList } from "@/pages/tokens";
import { formatClock, formatDateTime, formatMonthDay } from "@/lib/utils";
import { t } from "@/i18n";

const roles = ["admin", "operator", "viewer"] as const;

// shortDate：今天 08:12、昨天、10/28，不是今年的写全年份。
function shortDate(d: Date | undefined) {
  if (!d) return "—";
  const now = new Date();
  const days = Math.round((new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime() - new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()) / 86400000);
  if (days === 0) return `${t.members.today} ${formatClock(d)}`;
  if (days === 1) return t.members.yesterday;
  return formatMonthDay(d);
}

const roleOptions = roles.map((r) => (
  <option key={r} value={r}>
    {t.members.roles[r]}
  </option>
));

// 访问范围（见 docs/panel.md#访问范围）。
type Scope = { allProjects: boolean; allNodes: boolean; projectIds: string[]; nodeIds: string[] };
const fullScope: Scope = { allProjects: true, allNodes: true, projectIds: [], nodeIds: [] };

function scopeOf(u: PanelUser): Scope {
  return u.scope ? { allProjects: u.scope.allProjects, allNodes: u.scope.allNodes, projectIds: u.scope.projectIds, nodeIds: u.scope.nodeIds } : fullScope;
}

// memberSummary：第二行灰字。用户名（设置了显示名称时）、角色，限定了范围时再写范围。
function memberSummary(u: PanelUser) {
  const s = scopeOf(u);
  const parts = [u.displayName ? u.username : "", t.members.roles[u.role as (typeof roles)[number]] ?? u.role];
  if (u.role !== "admin" && !(s.allProjects && s.allNodes)) {
    parts.push(s.allProjects ? t.members.scope.allProjects : t.members.scope.projectCount(s.projectIds.length));
    parts.push(s.allNodes ? t.members.scope.allNodes : t.members.scope.nodeCount(s.nodeIds.length));
  }
  return parts.filter(Boolean).join(" · ");
}

// ScopePick：全部或指定的一组，指定时勾选。
function ScopePick({
  label,
  hint,
  all,
  ids,
  options,
  error,
  onRetry,
  empty,
  onChange,
}: {
  label: string;
  hint: string;
  all: boolean;
  ids: string[];
  options: { id: string; name: React.ReactNode }[] | undefined;
  // error：选项加载失败。
  error: unknown;
  onRetry: () => void;
  empty: string;
  onChange: (all: boolean, ids: string[]) => void;
}) {
  const picked = new Set(ids);
  return (
    <Field label={label} hint={hint}>
      <div className="grid">
        <div>
          <Segmented
            label={label}
            value={all ? "all" : "some"}
            onChange={(v) => onChange(v === "all", ids)}
            items={[
              { value: "all", label: t.members.scope.all },
              { value: "some", label: t.members.scope.some },
            ]}
          />
        </div>
        {/* 选「指定」时勾选列表展开，选「全部」时收起，下面的内容跟着平滑移动。 */}
        <Collapse open={!all}>
          <div className="pt-2">
            {!options && error ? (
              <QueryError error={error} onRetry={onRetry} />
            ) : !options ? (
              <RowsSkeleton count={1} />
            ) : options.length === 0 ? (
              <p className="text-muted-foreground">{empty}</p>
            ) : (
              <div className="grid max-h-44 gap-2 overflow-y-auto rounded-md border px-3 py-2.5">
                {options.map((o) => (
                  <Check key={o.id} checked={picked.has(o.id)} onChange={(v) => onChange(false, v ? [...ids, o.id] : ids.filter((x) => x !== o.id))}>
                    {o.name}
                  </Check>
                ))}
              </div>
            )}
          </div>
        </Collapse>
      </div>
    </Field>
  );
}

function ScopeFields({ scope, onChange }: { scope: Scope; onChange: (s: Scope) => void }) {
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}) });
  return (
    <>
      <ScopePick
        label={t.members.scope.projects}
        hint={t.members.scope.projectsHint}
        all={scope.allProjects}
        ids={scope.projectIds}
        empty={t.members.scope.noProjects}
        error={projects.error}
        onRetry={() => projects.refetch()}
        options={projects.data?.projects.map((p) => ({
          id: p.project!.id,
          name: (
            <>
              {p.project!.name}
              <span className="text-[13px] text-muted-foreground">{p.project!.networkName}</span>
            </>
          ),
        }))}
        onChange={(allProjects, projectIds) => onChange({ ...scope, allProjects, projectIds })}
      />
      <ScopePick
        label={t.members.scope.nodes}
        hint={t.members.scope.nodesHint}
        all={scope.allNodes}
        ids={scope.nodeIds}
        empty={t.members.scope.noNodes}
        error={nodes.error}
        onRetry={() => nodes.refetch()}
        options={nodes.data?.nodes.filter((n) => !n.pending).map((n) => ({ id: n.id, name: n.name }))}
        onChange={(allNodes, nodeIds) => onChange({ ...scope, allNodes, nodeIds })}
      />
    </>
  );
}

// EditAccess：角色和访问范围。参考 Apple「用户与群组」：列表只显示身份，修改都在详情里。
function EditAccess({ user, onDone, onCancel }: { user: PanelUser; onDone: () => void; onCancel: () => void }) {
  const [role, setRole] = useState(user.role);
  const [scope, setScope] = useState(() => scopeOf(user));
  const save = useMutation({
    mutationFn: () => settingsClient.updateUser({ id: user.id, role: role === user.role ? "" : role, scope: role === "admin" ? undefined : scope }),
    onSuccess: onDone,
  });
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <Field label={t.members.role} htmlFor="member-edit-role" hint={t.members.roleHints[role as (typeof roles)[number]]}>
        <Select id="member-edit-role" value={role} onChange={(e) => setRole(e.target.value)}>
          {roleOptions}
        </Select>
      </Field>
      {role !== "admin" && <ScopeFields scope={scope} onChange={setScope} />}
      {save.error && <Alert>{errorMessage(save.error)}</Alert>}
      <DialogActions>
        <Button variant="outline" onClick={onCancel}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={save.isPending}>
          {save.isPending && <Spinner />}
          {t.common.actions.save}
        </Button>
      </DialogActions>
    </form>
  );
}

function AddMember({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<string>("operator");
  const [scope, setScope] = useState(fullScope);
  const add = useMutation({
    mutationFn: () => settingsClient.createUser({ username, password, role, scope: role === "admin" ? fullScope : scope }),
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
      <Field label={t.members.username} htmlFor="member-name">
        <Input id="member-name" value={username} onChange={(e) => setUsername(e.target.value)} required maxLength={32} autoComplete="off" autoCapitalize="none" spellCheck={false} />
      </Field>
      <Field label={t.members.password} htmlFor="member-password" hint={t.members.passwordHint}>
        <Input id="member-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoComplete="new-password" />
      </Field>
      <Field label={t.members.role} htmlFor="member-role" hint={t.members.roleHints[role as (typeof roles)[number]]}>
        <Select id="member-role" value={role} onChange={(e) => setRole(e.target.value)}>
          {roleOptions}
        </Select>
      </Field>
      {role !== "admin" && <ScopeFields scope={scope} onChange={setScope} />}
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
      <Field label={t.members.newPassword} htmlFor="member-new-password" hint={t.members.passwordHint}>
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
  const [editing, setEditing] = useState<PanelUser | null>(null);
  const [tokensOf, setTokensOf] = useState<PanelUser | null>(null);
  const [error, setError] = useState("");
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["users"] });
  // 结果在列表上就能看到，不另外提示。
  const call = async (fn: () => Promise<unknown>) => {
    setError("");
    try {
      await fn();
    } catch (err) {
      setError(errorMessage(err));
    }
    refresh();
  };

  const addButton = (
    <Button variant="outline" onClick={() => setAdding(true)}>
      <Plus />
      {t.members.add}
    </Button>
  );
  return (
    <Section title={t.members.title} help={t.members.help} actions={addButton}>
      {error && <Alert>{error}</Alert>}
      {users.isPending ? (
        <RowsSkeleton />
      ) : !users.data ? (
        <QueryError error={errorMessage(users.error)} onRetry={() => users.refetch()} />
      ) : users.data.users.length === 0 ? (
        <EmptyRow action={addButton}>{t.members.empty}</EmptyRow>
      ) : (
        <List>
          {users.data.users.map((u) => (
            <Row
              key={String(u.id)}
              icon={<UserAvatar username={u.username} src={avatarSrc(u.avatar, u.avatarType)} className="-mt-0.5" />}
              center
              title={
                <>
                  <span className="truncate font-medium">{u.displayName || u.username}</span>
                  {u.isSelf && <span className="shrink-0 text-[13px] text-muted-foreground">{t.members.you}</span>}
                </>
              }
              sub={<span className="block truncate">{memberSummary(u)}</span>}
              // 只写开了两步验证的，每行都写「未开」是重复的噪音。
              cols={[{ content: u.totpEnabled ? <Status tone="success">{t.members.totpOn}</Status> : null, width: "w-28" }]}
              actionsWidth="w-9"
              actions={
                <RowMenu
                  className="size-9 last:mr-0 [&_svg]:size-5"
                  items={[
                    { label: t.members.editAccess, onSelect: () => setEditing(u) },
                    { label: t.tokens.title, onSelect: () => setTokensOf(u), readOnlyOk: true },
                    { label: t.members.resetPassword, onSelect: () => setResetting(u) },
                    ...(u.totpEnabled && !u.isSelf
                      ? [
                          {
                            label: t.members.clearTotp,
                            onSelect: async () => {
                              if (await confirmAction(t.members.clearTotpConfirm(u.username), { action: t.members.clearTotp, danger: true }))
                                call(() => settingsClient.updateUser({ id: u.id, clearTotp: true }));
                            },
                          },
                        ]
                      : []),
                    ...(!u.isSelf
                      ? [
                          {
                            label: t.members.deleteMember,
                            danger: true,
                            onSelect: async () => {
                              if (await confirmAction(t.members.deleteConfirm(u.username), { action: t.members.deleteMember, danger: true }))
                                call(() => settingsClient.deleteUser({ id: u.id }));
                            },
                          },
                        ]
                      : []),
                  ]}
                />
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
      <Dialog open={!!tokensOf} onClose={() => setTokensOf(null)} className="max-w-[560px]" title={t.tokens.title} description={tokensOf ? t.tokens.forUser(tokensOf.username) : undefined}>
        {tokensOf && <AccessTokenList userId={tokensOf.id} />}
      </Dialog>
      <Dialog open={!!editing} onClose={() => setEditing(null)} title={t.members.editAccess} description={editing?.username}>
        {editing && (
          <EditAccess
            user={editing}
            onCancel={() => setEditing(null)}
            onDone={() => {
              setEditing(null);
              refresh();
            }}
          />
        )}
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
