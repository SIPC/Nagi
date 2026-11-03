import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { renderSVG } from "uqr";
import { toast } from "sonner";
import { authClient, errorMessage, settingsClient } from "@/lib/api";
import { confirmAction } from "@/components/confirm";
import { Alert, Button, CopyButton, Dialog, DialogActions, Field, Input, QueryError, RowsSkeleton, Section, SettingRow, SettingRows, Spinner } from "@/components/ui";
import { t } from "@/i18n";
import { SessionList } from "@/pages/members";
import { AccessTokenSection } from "@/pages/tokens";
import { CopyTextButton } from "@/pages/access";
import { CodeBox } from "@/pages/invite";
import { useRole } from "@/components/role";

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
        <Field label={t.security.password.newPassword} htmlFor="sec-next">
          <Input id="sec-next" type="password" placeholder={t.security.password.hint} autoComplete="new-password" required minLength={8} value={next} onChange={(e) => setNext(e.target.value)} />
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
    <Dialog open onClose={onClose} title={t.security.totp.enable} description={t.security.totp.scanHint}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          enable.mutate();
        }}
      >
        {/* 二维码固定黑底白字，深色模式下也能扫。 */}
        <div className="size-32 rounded-md bg-white p-2 [&_svg]:size-full" role="img" aria-label={t.security.totp.qrCode} dangerouslySetInnerHTML={{ __html: renderSVG(setup.uri, { border: 1 }) }} />
        <CodeBox text={setup.secret} />
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
            {t.security.totp.confirmEnable}
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
      toast.success(t.security.totp.disabledMsg);
      onDone();
      onClose();
    },
  });
  return (
    <Dialog open onClose={onClose} title={t.security.totp.disable}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          disable.mutate();
        }}
      >
        <Field label={t.security.totp.disableLabel} htmlFor="totp-disable" help={t.security.totp.disableHelp}>
          <Input
            id="totp-disable"
            type={/^\d{0,6}$/.test(code) ? "text" : "password"}
            autoComplete="off"
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

// 账号安全：登录密码、两步验证、安全入口（管理员）和登录设备。一项一行，表单放进对话框。
export function SecurityCard() {
  const queryClient = useQueryClient();
  const sec = useQuery({
    queryKey: ["security"],
    queryFn: () => settingsClient.getSecurity({}),
  });
  const { isAdmin } = useRole();
  // 主密钥的位置在面板设置里，只有管理员查得到。
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}), enabled: isAdmin });
  const masterKeyPath = settings.data?.settings?.masterKeyPath;
  const [dialog, setDialog] = useState<"password" | "disable-totp" | null>(null);
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(null);
  const [busy, setBusy] = useState<"totp" | "entrance" | null>(null);
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
      setSetup(await settingsClient.beginTOTP({}));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const setEntrance = async (enabled: boolean) => {
    if (!enabled && !(await confirmAction(t.security.entrance.disableConfirm, { action: t.security.entrance.actionOff, danger: true }))) return;
    setBusy("entrance");
    setError("");
    try {
      await settingsClient.setEntrance({ enabled });
      toast.success(enabled ? t.security.entrance.enabledMsg : t.security.entrance.disabledMsg);
      await sec.refetch();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const s = sec.data;
  const entranceUrl = s?.entrance ? `${window.location.origin}/${s.entrance}` : "";

  return (
    <>
      <Section title={<span title={t.settings.tabs.security.description}>{t.settings.tabs.security.label}</span>}>
        {error && <Alert>{error}</Alert>}
        {sec.isPending ? (
          <RowsSkeleton />
        ) : !s ? (
          <QueryError error={errorMessage(sec.error)} onRetry={() => sec.refetch()} />
        ) : (
          <SettingRows>
            <SettingRow label={t.security.password.label} description={<span title={t.security.password.help}>{t.security.password.description}</span>}>
              <Button variant="outline" onClick={() => setDialog("password")}>
                {t.security.password.submit}
              </Button>
            </SettingRow>
            <SettingRow label={t.security.totp.label} description={<span title={t.security.totp.help}>{s.totpEnabled ? t.security.totp.onStatus : t.security.totp.offStatus}</span>}>
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
            {isAdmin && masterKeyPath && (
              <SettingRow
                label={t.security.masterKey.label}
                description={<span className="break-all" title={`${t.security.masterKey.description} ${t.security.masterKey.help}`}>{`${masterKeyPath} · ${t.security.masterKey.backup}`}</span>}
              >
                <CopyTextButton text={masterKeyPath} label={t.security.masterKey.copyPath} />
              </SettingRow>
            )}
            {isAdmin && (
              <SettingRow
                label={t.security.entrance.label}
                description={<span className="break-all" title={t.security.entrance.help}>{s.entrance ? `${entranceUrl} · ${t.security.entrance.onStatus}` : t.security.entrance.offStatus}</span>}
              >
                <span className="flex items-center gap-2">
                  {s.entrance && <CopyButton text={entranceUrl} />}
                  <Button variant="outline" onClick={() => setEntrance(!s.entrance)} disabled={busy !== null}>
                    {busy === "entrance" && <Spinner />}
                    {s.entrance ? t.security.entrance.actionOff : t.security.entrance.actionOn}
                  </Button>
                </span>
              </SettingRow>
            )}
          </SettingRows>
        )}
      </Section>
      <Section title={<span title={t.security.sessions.description}>{t.security.sessions.label}</span>}>
        <SessionList />
      </Section>
      <AccessTokenSection />
      {dialog === "password" && <PasswordDialog onClose={() => setDialog(null)} onDone={refresh} />}
      {dialog === "disable-totp" && <DisableTotpDialog onClose={() => setDialog(null)} onDone={refresh} />}
      {setup && <EnableTotpDialog setup={setup} onClose={() => setSetup(null)} onDone={refresh} />}
    </>
  );
}
