import { useEffect, useState } from "react";
import { Plus } from "lucide-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { renderSVG } from "uqr";
import { toast } from "sonner";
import { authClient, errorMessage, settingsClient, toDate } from "@/lib/api";
import { addPasskey, cancelled, defaultPasskeyName, passkeySupported } from "@/lib/passkey";
import { formatDate, formatDateTime, timeAgo } from "@/lib/utils";
import type { CreateAppPairingResponse, Passkey, Security } from "@/gen/nagi/v1/api_pb";
import { confirmAction } from "@/components/confirm";
import { isElevationRequired, requireElevation } from "@/components/elevate";
import { Alert, Button, ButtonLink, Dialog, DialogActions, EmptyRow, Field, Input, List, QueryError, Row, RowsSkeleton, Section, SettingRow, SettingRows, Spinner, CodeBlock, textLink } from "@/components/ui";
import { t } from "@/i18n";
import { SessionList } from "@/pages/members";
import { AccessTokenSection } from "@/pages/tokens";
import { useRole } from "@/components/role";
import { repo } from "@/pages/about";

function PasswordDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [cur, setCur] = useState("");
  const [next, setNext] = useState("");
  const change = useMutation({
    mutationFn: () => authClient.changePassword({ current: cur, newPassword: next }),
    onSuccess: () => {
      toast.success(t.security.password.changed);
      onDone();
      onClose();
    },
  });
  return (
    <Dialog open onClose={onClose} title={t.security.password.submit}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          change.mutate();
        }}
      >
        <Field label={t.security.password.current} htmlFor="sec-cur">
          <Input id="sec-cur" type="password" autoComplete="current-password" autoFocus required value={cur} onChange={(e) => setCur(e.target.value)} />
        </Field>
        <Field label={t.security.password.newPassword} htmlFor="sec-next" hint={t.security.password.hint}>
          <Input id="sec-next" type="password" autoComplete="new-password" required minLength={8} value={next} onChange={(e) => setNext(e.target.value)} />
        </Field>
        {change.error && <Alert>{errorMessage(change.error)}</Alert>}
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={change.isPending}>
            {change.isPending && <Spinner />}
            {t.security.password.submit}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

function EnableTotpDialog({ setup, onClose, onDone }: { setup: { secret: string; uri: string }; onClose: () => void; onDone: () => void }) {
  const [code, setCode] = useState("");
  const enable = useMutation({
    mutationFn: () => settingsClient.enableTOTP({ code }),
    onSuccess: () => {
      toast.success(t.security.totp.enabledMsg);
      onDone();
      onClose();
    },
  });
  return (
    <Dialog open onClose={onClose} title={t.security.totp.enableTitle} description={t.security.totp.scanHint}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          enable.mutate();
        }}
      >
        {/* 二维码固定黑底白字，深色模式下也能扫。 */}
        <div className="size-32 rounded-md bg-white p-2 [&_svg]:size-full" role="img" aria-label={t.security.totp.qrCode} dangerouslySetInnerHTML={{ __html: renderSVG(setup.uri, { border: 1 }) }} />
        <CodeBlock copy nowrap>{setup.secret}</CodeBlock>
        <Field label={t.security.totp.code} htmlFor="totp-enable">
          <Input id="totp-enable" inputMode="numeric" autoComplete="one-time-code" placeholder="000000" value={code} onChange={(e) => setCode(e.target.value)} />
        </Field>
        {enable.error && <Alert>{errorMessage(enable.error)}</Alert>}
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={enable.isPending}>
            {enable.isPending && <Spinner />}
            {t.security.totp.enable}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

function DisableTotpDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [code, setCode] = useState("");
  const disable = useMutation({
    // 6 位数字按验证码，其余按登录密码。
    mutationFn: () => settingsClient.disableTOTP(/^\d{6}$/.test(code.trim()) ? { code: code.trim() } : { password: code }),
    onSuccess: () => {
      onDone();
      onClose();
    },
  });
  return (
    <Dialog open onClose={onClose} title={t.security.totp.disableTitle}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          disable.mutate();
        }}
      >
        {/* 可能是登录密码，始终按密码显示：按内容切换明文时，以数字开头的密码前几位会露出来。 */}
        <Field label={t.security.totp.disableLabel} htmlFor="totp-disable" help={t.security.totp.disableHelp}>
          <Input
            id="totp-disable"
            type="password"
            autoComplete="off"
            data-1p-ignore
            data-lpignore="true"
            autoFocus
            placeholder={t.security.totp.disablePlaceholder}
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
        </Field>
        {disable.error && <Alert>{errorMessage(disable.error)}</Alert>}
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" variant="destructive" disabled={disable.isPending}>
            {disable.isPending && <Spinner />}
            {t.security.totp.disable}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

function AddPasskeyDialog({ onClose, onDone }: { onClose: () => void; onDone: (sec: Security) => void }) {
  const [name, setName] = useState(defaultPasskeyName);
  const add = useMutation({
    mutationFn: async () => {
      try {
        return await addPasskey(name);
      } catch (err) {
        // 添加前要先再验证一次身份。
        if (!isElevationRequired(err) || !(await requireElevation(t.security.passkeys.elevate, { suggestTotp: false, passwordOnly: true }))) throw err;
        return addPasskey(name);
      }
    },
    onSuccess: (sec) => {
      toast.success(t.security.passkeys.added);
      onDone(sec);
      onClose();
    },
  });
  return (
    <Dialog open onClose={onClose} title={t.security.passkeys.add} description={t.security.passkeys.help}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          add.mutate();
        }}
      >
        <Field label={t.security.passkeys.name} htmlFor="passkey-name" hint={t.security.passkeys.nameHint}>
          <Input id="passkey-name" autoFocus required maxLength={50} value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        {add.error && !cancelled(add.error) && <Alert>{errorMessage(add.error)}</Alert>}
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={add.isPending}>
            {add.isPending && <Spinner />}
            {t.security.passkeys.add}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// 通行密钥（见 docs/panel.md#通行密钥）：只能在绑定的域名下添加和使用。
function PasskeySection({ sec, onChange }: { sec: Security; onChange: (sec: Security) => void }) {
  const p = t.security.passkeys;
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");
  const domain = sec.passkeyDomain;
  const usable = passkeySupported(domain);
  const remove = async (k: Passkey) => {
    if (!(await confirmAction(p.removeConfirm(k.name), { action: p.removeAction, danger: true }))) return;
    setError("");
    try {
      onChange(await settingsClient.deletePasskey({ id: k.id }));
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  const { isAdmin } = useRole();
  const addButton = usable ? (
    <Button variant="outline" onClick={() => setAdding(true)}>
      <Plus />
      {p.add}
    </Button>
  ) : !domain && isAdmin ? (
    <ButtonLink variant="outline" to="/settings" search={{ tab: "access" }} readOnlyOk>
      {p.bindDomain}
    </ButtonLink>
  ) : undefined;
  // 没绑定域名，或者不是从绑定的域名打开的：说明在哪里才能添加。
  const unavailable = !domain ? p.noDomain : !usable ? p.otherHost(`https://${domain}`) : "";
  return (
    <Section title={p.label} help={p.help} actions={sec.passkeys.length > 0 && usable ? addButton : undefined}>
      {error && <Alert>{error}</Alert>}
      {sec.passkeys.length === 0 ? (
        <EmptyRow action={addButton}>{unavailable || p.empty}</EmptyRow>
      ) : (
        <List>
          {sec.passkeys.map((k) => {
            const created = toDate(k.createdAt);
            const used = toDate(k.lastUsedAt);
            return (
              <Row
                key={String(k.id)}
                title={<span className="truncate">{k.name}</span>}
                sub={<span className="block truncate">{k.otherDomain ? p.otherDomain : p.addedAt(formatDate(created) ?? "")}</span>}
                cols={[
                  {
                    content: <span title={used ? formatDateTime(used) : undefined}>{used ? p.lastUsed(timeAgo(used)) : p.neverUsed}</span>,
                    width: "w-36",
                  },
                ]}
                actions={
                  <Button size="row" variant="ghost" onClick={() => remove(k)}>
                    {p.remove}
                  </Button>
                }
              />
            );
          })}
        </List>
      )}
      {/* 已有通行密钥但在这里加不了时，写一行原因（标题栏没有「添加」）。 */}
      {sec.passkeys.length > 0 && unavailable && <p className="leading-5 text-muted-foreground">{unavailable}</p>}
      {adding && <AddPasskeyDialog onClose={() => setAdding(false)} onDone={onChange} />}
    </Section>
  );
}

// ---- 手机 App（见 docs/android.md#扫码） ----

// 配对码 5 分钟有效。本机时钟和面板不一致时，按收到时算 5 分钟。
const pairingTtlMs = 5 * 60_000;

type Pairing = { code: string; uri: string; deadline: number };

async function createPairing(): Promise<Pairing> {
  let res: CreateAppPairingResponse;
  try {
    res = await authClient.createAppPairing({});
  } catch (err) {
    // 生成配对码前要先再验证一次身份，只认密码或验证码。
    if (!isElevationRequired(err) || !(await requireElevation(t.security.app.elevate, { suggestTotp: false, passwordOnly: true }))) throw err;
    res = await authClient.createAppPairing({});
  }
  const now = Date.now();
  const left = (toDate(res.expiresAt)?.getTime() ?? 0) - now;
  return { code: res.code, uri: res.uri, deadline: now + (left > 0 && left <= pairingTtlMs + 30_000 ? left : pairingTtlMs) };
}

function AddPhoneDialog({ initial, onClose }: { initial: Pairing; onClose: () => void }) {
  const a = t.security.app;
  const queryClient = useQueryClient();
  const [pairing, setPairing] = useState(initial);
  const [now, setNow] = useState(Date.now);
  const left = Math.max(0, Math.ceil((pairing.deadline - now) / 1000));
  const status = useQuery({
    queryKey: ["app-pairing", pairing.code],
    queryFn: () => authClient.getAppPairing({ code: pairing.code }),
    refetchInterval: 2000,
    enabled: left > 0,
    gcTime: 0,
  });
  const expired = left === 0 || !!status.data?.expired;
  const used = status.data?.used ? status.data.deviceName : undefined;

  useEffect(() => {
    if (expired) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [expired]);

  useEffect(() => {
    if (used === undefined) return;
    toast.success(a.added(used));
    queryClient.invalidateQueries({ queryKey: ["sessions"] });
    onClose();
  }, [used]);

  const regenerate = useMutation({
    mutationFn: createPairing,
    onSuccess: (next) => {
      setPairing(next);
      setNow(Date.now());
    },
  });

  return (
    <Dialog open onClose={onClose} title={a.add} description={a.scan} className="max-w-[440px]">
      <div className="grid justify-items-center gap-4">
        {expired ? (
          <div className="grid size-56 place-content-center justify-items-center gap-3 rounded-md border text-muted-foreground">
            {a.expired}
            <Button variant="outline" onClick={() => regenerate.mutate()} disabled={regenerate.isPending}>
              {regenerate.isPending && <Spinner />}
              {a.regenerate}
            </Button>
          </div>
        ) : (
          // 二维码固定白底黑块，深色模式下也能扫。
          <div
            className="size-56 rounded-md bg-white p-2 [&_svg]:size-full [&_svg]:[shape-rendering:crispEdges]"
            role="img"
            aria-label={a.qrCode}
            dangerouslySetInnerHTML={{ __html: renderSVG(pairing.uri, { border: 2 }) }}
          />
        )}
        {!expired && <p className="text-muted-foreground">{`${a.expiresIn(left)} · ${a.singleUse}`}</p>}
      </div>
      {regenerate.error && <Alert>{errorMessage(regenerate.error)}</Alert>}
      <DialogActions className="justify-between">
        <span className="text-muted-foreground max-md:hidden">
          {a.noApp}{" "}
          <a className={textLink} href={`${repo}/releases`} target="_blank" rel="noreferrer">
            {a.download}
          </a>
        </span>
        <Button onClick={onClose}>{t.common.actions.done}</Button>
      </DialogActions>
    </Dialog>
  );
}

function AppSection() {
  const a = t.security.app;
  const [pairing, setPairing] = useState<Pairing | null>(null);
  const create = useMutation({ mutationFn: createPairing, onSuccess: setPairing });
  return (
    <Section
      title={a.label}
      help={a.help}
      description={a.description}
      actions={
        <Button variant="outline" onClick={() => create.mutate()} disabled={create.isPending}>
          {create.isPending ? <Spinner /> : <Plus />}
          {a.add}
        </Button>
      }
    >
      {create.error && <Alert>{errorMessage(create.error)}</Alert>}
      {pairing && <AddPhoneDialog initial={pairing} onClose={() => setPairing(null)} />}
    </Section>
  );
}

// 账号安全：登录密码、两步验证、通行密钥、登录设备和访问令牌。一项一行，表单放进对话框。
// 主密钥和安全入口是面板的设置，在「面板访问」（ui.md 设置页）。
export function SecurityCard() {
  const queryClient = useQueryClient();
  const sec = useQuery({
    queryKey: ["security"],
    queryFn: () => settingsClient.getSecurity({}),
  });
  const [dialog, setDialog] = useState<"password" | "disable-totp" | null>(null);
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(null);
  const [busy, setBusy] = useState<"totp" | null>(null);
  const [error, setError] = useState("");

  // 项目页的两步验证提醒读 me，登录设备会因为退出其他会话而变化。
  const refresh = () => {
    sec.refetch();
    queryClient.invalidateQueries({ queryKey: ["me"] });
    queryClient.invalidateQueries({ queryKey: ["sessions"] });
  };

  const beginTotp = async () => {
    setBusy("totp");
    setError("");
    try {
      try {
        setSetup(await settingsClient.beginTOTP({}));
      } catch (err) {
        // 开启前要先再验证一次身份。
        if (!isElevationRequired(err) || !(await requireElevation(t.security.totp.elevate, { suggestTotp: false, passwordOnly: true }))) throw err;
        setSetup(await settingsClient.beginTOTP({}));
      }
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const s = sec.data;

  return (
    <>
      <Section title={t.settings.tabs.security.label}>
        {error && <Alert>{error}</Alert>}
        {sec.isPending ? (
          <RowsSkeleton />
        ) : !s ? (
          <QueryError error={errorMessage(sec.error)} onRetry={() => sec.refetch()} />
        ) : (
          <SettingRows>
            <SettingRow label={t.security.password.label} help={t.security.password.help} description={t.security.password.description}>
              <Button variant="outline" onClick={() => setDialog("password")}>
                {t.security.password.submit}
              </Button>
            </SettingRow>
            <SettingRow label={t.security.totp.label} help={t.security.totp.help} description={s.totpEnabled ? t.security.totp.onStatus : t.security.totp.offStatus}>
              {s.totpEnabled ? (
                <Button variant="outline" onClick={() => setDialog("disable-totp")}>
                  {t.security.totp.actionOff}
                </Button>
              ) : (
                <Button variant="outline" onClick={beginTotp} disabled={busy !== null}>
                  {busy === "totp" && <Spinner />}
                  {t.security.totp.actionOn}
                </Button>
              )}
            </SettingRow>
          </SettingRows>
        )}
      </Section>
      {s && <PasskeySection sec={s} onChange={(next) => queryClient.setQueryData(["security"], next)} />}
      <AppSection />
      <Section title={t.security.sessions.label} help={t.security.sessions.help}>
        <SessionList />
      </Section>
      <AccessTokenSection />
      {dialog === "password" && <PasswordDialog onClose={() => setDialog(null)} onDone={refresh} />}
      {dialog === "disable-totp" && <DisableTotpDialog onClose={() => setDialog(null)} onDone={refresh} />}
      {setup && <EnableTotpDialog setup={setup} onClose={() => setSetup(null)} onDone={refresh} />}
    </>
  );
}
