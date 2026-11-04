// 访问令牌（MCP 接入，见 docs/mcp.md）：账号安全里的令牌列表、生成令牌，管理员查看成员的令牌，OAuth 授权页。
import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { toast } from "sonner";
import { confirmAction } from "@/components/confirm";
import { isElevationRequired, requireElevation } from "@/components/elevate";
import { useRole } from "@/components/role";
import { Alert, Button, Check, CodeBlock, CopyButton, Dialog, DialogActions, EmptyRow, Field, Input, List, QueryError, Row, RowsSkeleton, Section, Select, Spinner } from "@/components/ui";
import type { AccessToken, CreateAccessTokenResponse } from "@/gen/nagi/v1/api_pb";
import { authClient, errorMessage, settingsClient, toDate } from "@/lib/api";
import { formatDate, formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { AuthShell, returnToKey } from "@/pages/auth";

const expiryDays = [30, 90, 365, 0];

// mcpUrl：MCP 的地址。开了安全入口时要带着入口（见 docs/mcp.md#入口），入口只有管理员知道。
function useMcpUrl() {
  const sec = useQuery({ queryKey: ["security"], queryFn: () => settingsClient.getSecurity({}) });
  const s = sec.data;
  if (!s) return { url: "", hidden: false };
  if (s.entranceEnabled && !s.entrance) return { url: "", hidden: true };
  return { url: `${window.location.origin}${s.entrance ? "/" + s.entrance : ""}/mcp`, hidden: false };
}

function tokenSummary(tk: AccessToken) {
  const k = t.tokens;
  const expires = toDate(tk.expiresAt);
  const parts = [
    tk.source === "oauth" ? k.sourceOAuth : k.sourcePersonal,
    tk.allowWrite ? k.write : k.readOnly,
    ...(tk.allowExec ? [k.exec] : []),
    ...(tk.allowNodeExec ? [k.nodeExec] : []),
    !expires ? k.neverExpires : expires.getTime() < Date.now() ? k.expired : k.expiresOn(formatDate(expires) ?? ""),
  ];
  return parts.join(" · ");
}

// AccessTokenList：令牌列表。userId 不为 0 时是管理员看别人的。
export function AccessTokenList({ userId = 0n }: { userId?: bigint }) {
  const queryClient = useQueryClient();
  const key = ["access-tokens", String(userId)];
  const tokens = useQuery({ queryKey: key, queryFn: () => settingsClient.listAccessTokens({ userId }) });
  const [error, setError] = useState("");
  const revoke = async (tk: AccessToken) => {
    if (!(await confirmAction(t.tokens.revokeConfirm(tk.name), { action: t.tokens.revoke, danger: true }))) return;
    setError("");
    try {
      await settingsClient.revokeAccessToken({ id: tk.id });
      toast.success(t.tokens.revoked);
    } catch (err) {
      setError(errorMessage(err));
    }
    queryClient.invalidateQueries({ queryKey: key });
  };
  if (tokens.isPending) return <RowsSkeleton />;
  if (!tokens.data) return <QueryError error={errorMessage(tokens.error)} onRetry={() => tokens.refetch()} />;
  return (
    <>
      {error && <Alert>{error}</Alert>}
      {tokens.data.tokens.length === 0 ? (
        <EmptyRow>{t.tokens.empty}</EmptyRow>
      ) : (
        <List>
          {tokens.data.tokens.map((tk) => {
            const used = toDate(tk.lastUsedAt);
            return (
              <Row
                key={String(tk.id)}
                title={<span className="truncate">{tk.name}</span>}
                sub={<span className="block truncate">{tokenSummary(tk)}</span>}
                cols={[
                  {
                    content: <span title={used ? `${formatDateTime(used)} · ${tk.lastIp}` : undefined}>{used ? t.tokens.lastUsed(timeAgo(used)) : t.tokens.neverUsed}</span>,
                    width: "w-36",
                  },
                ]}
                actions={
                  <Button size="row" variant="ghost" onClick={() => revoke(tk)}>
                    {t.tokens.revoke}
                  </Button>
                }
              />
            );
          })}
        </List>
      )}
    </>
  );
}

function CreateToken({ onDone, onCancel }: { onDone: (res: CreateAccessTokenResponse) => void; onCancel: () => void }) {
  const { isViewer, isAdmin } = useRole();
  const [name, setName] = useState("");
  const [days, setDays] = useState(90);
  const [write, setWrite] = useState(false);
  const [exec, setExec] = useState(false);
  const [nodeExec, setNodeExec] = useState(false);
  const create = useMutation({
    mutationFn: async () => {
      const req = { name, days, allowWrite: write, allowExec: exec, allowNodeExec: isAdmin && nodeExec };
      try {
        return await settingsClient.createAccessToken(req);
      } catch (err) {
        // 生成令牌要先再验证一次身份。
        if (!isElevationRequired(err) || !(await requireElevation(t.tokens.elevate))) throw err;
        return settingsClient.createAccessToken(req);
      }
    },
    onSuccess: onDone,
  });
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate();
      }}
    >
      <Field label={t.tokens.name} htmlFor="token-name" required>
        <Input id="token-name" value={name} onChange={(e) => setName(e.target.value)} placeholder={t.tokens.namePlaceholder} required maxLength={64} autoComplete="off" />
      </Field>
      <Field label={t.tokens.expires} htmlFor="token-expires">
        <Select id="token-expires" value={String(days)} onChange={(e) => setDays(Number(e.target.value))}>
          {expiryDays.map((d) => (
            <option key={d} value={String(d)}>
              {t.tokens.expiryOptions[d]}
            </option>
          ))}
        </Select>
      </Field>
      {!isViewer && (
        <div className="grid gap-2">
          <Check checked={write} onChange={setWrite} help={t.tokens.allowWriteHelp}>
            {t.tokens.allowWrite}
          </Check>
          <Check
            checked={exec}
            onChange={(v) => {
              setExec(v);
              if (!v) setNodeExec(false);
            }}
            help={t.tokens.allowExecHelp}
          >
            {t.tokens.allowExec}
          </Check>
          {isAdmin && (
            <Check
              checked={nodeExec}
              onChange={(v) => {
                // 节点终端也是执行命令，一起勾上。
                setNodeExec(v);
                if (v) setExec(true);
              }}
              help={t.tokens.allowNodeExecHelp}
            >
              {t.tokens.allowNodeExec}
            </Check>
          )}
        </div>
      )}
      {create.error && <Alert>{errorMessage(create.error)}</Alert>}
      <DialogActions>
        <Button variant="outline" onClick={onCancel}>
          {t.common.actions.cancel}
        </Button>
        <Button type="submit" disabled={create.isPending}>
          {create.isPending && <Spinner />}
          {t.tokens.create}
        </Button>
      </DialogActions>
    </form>
  );
}

function CreatedToken({ secret, url, onDone }: { secret: string; url: string; onDone: () => void }) {
  return (
    <div className="grid gap-4">
      <p className="text-muted-foreground">{t.tokens.createdHint}</p>
      <CodeBlock copy>{secret}</CodeBlock>
      {url && (
        <div className="grid gap-2">
          <span className="text-[13px] text-muted-foreground">{t.tokens.setupClaudeCode}</span>
          <CodeBlock copy>{`claude mcp add --transport http nagi ${url} --header "Authorization: Bearer ${secret}"`}</CodeBlock>
          <span className="text-[13px] text-muted-foreground">{t.tokens.setupOther}</span>
          <CodeBlock copy>{`Authorization: Bearer ${secret}`}</CodeBlock>
        </div>
      )}
      <DialogActions>
        <Button onClick={onDone}>{t.tokens.done}</Button>
      </DialogActions>
    </div>
  );
}

// AccessTokenSection：账号安全里的「访问令牌」。
export function AccessTokenSection() {
  const queryClient = useQueryClient();
  const mcp = useMcpUrl();
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<CreateAccessTokenResponse | null>(null);
  return (
    <Section
      title={t.tokens.title}
      help={t.tokens.help}
      actions={
        <Button variant="outline" onClick={() => setCreating(true)}>
          <Plus />
          {t.tokens.create}
        </Button>
      }
    >
      <List>
        <Row
          center
          title={t.tokens.mcpUrl}
          sub={<span className="block truncate" title={mcp.url ? t.tokens.mcpUrlHint : undefined}>{mcp.hidden ? t.tokens.mcpUrlEntrance : mcp.url || "—"}</span>}
          actions={mcp.url ? <CopyButton text={mcp.url} /> : null}
        />
      </List>
      <AccessTokenList />
      <Dialog open={creating} onClose={() => setCreating(false)} title={t.tokens.create} description={t.tokens.help}>
        <CreateToken
          onCancel={() => setCreating(false)}
          onDone={(res) => {
            setCreating(false);
            setCreated(res);
            queryClient.invalidateQueries({ queryKey: ["access-tokens"] });
          }}
        />
      </Dialog>
      <Dialog open={!!created} onClose={() => setCreated(null)} title={t.tokens.created} description={created?.token?.name}>
        {created && <CreatedToken secret={created.secret} url={mcp.url} onDone={() => setCreated(null)} />}
      </Dialog>
    </Section>
  );
}

// ---- OAuth 授权页 ----

function oauthRequest(search: URLSearchParams) {
  const g = (k: string) => search.get(k) ?? "";
  return {
    responseType: g("response_type"),
    clientId: g("client_id"),
    redirectUri: g("redirect_uri"),
    state: g("state"),
    codeChallenge: g("code_challenge"),
    codeChallengeMethod: g("code_challenge_method"),
    scope: g("scope"),
    resource: g("resource"),
  };
}

export function OAuthAuthorizePage() {
  const request = oauthRequest(new URLSearchParams(window.location.search));
  const me = useQuery({ queryKey: ["me"], queryFn: () => authClient.me({}), retry: false });
  const check = useQuery({ queryKey: ["oauth-request", window.location.search], queryFn: () => authClient.getOAuthRequest(request), enabled: !!me.data, retry: false });
  const [days, setDays] = useState(90);
  const [write, setWrite] = useState(false);
  const [exec, setExec] = useState(false);
  const [nodeExec, setNodeExec] = useState(false);
  const [leaving, setLeaving] = useState(false);
  const approve = useMutation({
    mutationFn: (allow: boolean) => authClient.approveOAuth({ request, allow, days, allowWrite: write, allowExec: exec, allowNodeExec: nodeExec }),
    onSuccess: (res) => {
      setLeaving(true);
      window.location.assign(res.redirectUrl);
    },
  });

  // 没登录：记下这个地址，登录后回来。
  useEffect(() => {
    if (me.error) {
      try {
        sessionStorage.setItem(returnToKey, window.location.pathname + window.location.search);
      } catch {
        // 存不了就只能登录后重新从客户端发起授权。
      }
      window.location.assign("/login");
    }
  }, [me.error]);

  const k = t.tokens.oauth;
  if (me.isPending || me.error || (check.isPending && !check.error))
    return (
      <AuthShell title={k.title}>
        <RowsSkeleton count={2} />
      </AuthShell>
    );
  if (!check.data)
    return (
      <AuthShell title={k.invalid}>
        <Alert>{errorMessage(check.error)}</Alert>
      </AuthShell>
    );
  const c = check.data;
  const viewer = c.role === "viewer";
  return (
    <AuthShell title={k.request(c.clientName)} description={k.redirect(c.redirectHost)}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          approve.mutate(true);
        }}
      >
        <p className="text-muted-foreground">{k.account(me.data?.displayName || me.data?.username || "")}</p>
        <Field label={t.tokens.expires} htmlFor="oauth-expires">
          <Select id="oauth-expires" value={String(days)} onChange={(e) => setDays(Number(e.target.value))}>
            {expiryDays.map((d) => (
              <option key={d} value={String(d)}>
                {t.tokens.expiryOptions[d]}
              </option>
            ))}
          </Select>
        </Field>
        {!viewer && (
          <div className="grid gap-2">
            <Check checked={write} onChange={setWrite} help={t.tokens.allowWriteHelp}>
              {t.tokens.allowWrite}
            </Check>
            <Check
              checked={exec}
              onChange={(v) => {
                setExec(v);
                if (!v) setNodeExec(false);
              }}
              help={t.tokens.allowExecHelp}
            >
              {t.tokens.allowExec}
            </Check>
            {c.role === "admin" && (
              <Check
                checked={nodeExec}
                onChange={(v) => {
                  setNodeExec(v);
                  if (v) setExec(true);
                }}
                help={t.tokens.allowNodeExecHelp}
              >
                {t.tokens.allowNodeExec}
              </Check>
            )}
          </div>
        )}
        {approve.error && <Alert>{errorMessage(approve.error)}</Alert>}
        <div className="grid grid-cols-2 gap-2">
          <Button variant="outline" disabled={approve.isPending || leaving} onClick={() => approve.mutate(false)}>
            {k.deny}
          </Button>
          <Button type="submit" disabled={approve.isPending || leaving}>
            {(approve.isPending || leaving) && <Spinner />}
            {k.allow}
          </Button>
        </div>
      </form>
    </AuthShell>
  );
}
