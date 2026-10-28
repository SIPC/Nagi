import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { renderSVG } from "uqr";
import { authClient, errorMessage, settingsClient } from "@/lib/api";
import { Alert, Badge, Button, CodeBlock, Field, Input, SettingRow, SettingRows } from "@/components/ui";
import { t } from "@/i18n";

type Scope = "password" | "totp" | "entrance";

// 账号安全：面板主密钥、登录密码、两步验证、安全入口，一项一行。
export function SecurityCard() {
  const sec = useQuery({
    queryKey: ["security"],
    queryFn: () => settingsClient.getSecurity({}),
  });
  const [cur, setCur] = useState("");
  const [next, setNext] = useState("");
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(null);
  const [code, setCode] = useState("");
  // 提示和错误显示在触发它的那一项里。
  const [msg, setMsg] = useState<{ scope: Scope; text: string } | null>(null);
  const [error, setError] = useState<{ scope: Scope; text: string } | null>(null);

  const run = async (scope: Scope, fn: () => Promise<unknown>, ok: string) => {
    setError(null);
    setMsg(null);
    try {
      await fn();
      if (ok) setMsg({ scope, text: ok });
      sec.refetch();
    } catch (err) {
      setError({ scope, text: errorMessage(err) });
    }
  };

  const feedback = (scope: Scope) => (
    <>
      {error?.scope === scope && <Alert>{error.text}</Alert>}
      {msg?.scope === scope && <p className="text-[13px] text-success">{msg.text}</p>}
    </>
  );

  const s = sec.data;
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}) });
  const keyPath = settings.data?.settings?.masterKeyPath;

  return (
    <SettingRows>
      {keyPath && (
        <SettingRow
          label={t.security.masterKey.label}
          description={t.security.masterKey.description}
          help={t.security.masterKey.help}
        >
          <CodeBlock copy>{keyPath}</CodeBlock>
        </SettingRow>
      )}

      <SettingRow label={t.security.password.label} description={t.security.password.description} help={t.security.password.help}>
        <div className="grid max-w-sm gap-3">
          {feedback("password")}
          <Field label={t.security.password.current} htmlFor="sec-cur">
            <Input id="sec-cur" type="password" value={cur} onChange={(e) => setCur(e.target.value)} />
          </Field>
          <Field label={t.security.password.newPassword} htmlFor="sec-next" hint={t.security.password.hint}>
            <Input id="sec-next" type="password" value={next} onChange={(e) => setNext(e.target.value)} />
          </Field>
          <div>
            <Button
              size="sm"
              onClick={() =>
                run(
                  "password",
                  () =>
                    authClient.changePassword({
                      current: cur,
                      newPassword: next,
                    }),
                  t.security.password.changed,
                )
              }
            >
              {t.security.password.submit}
            </Button>
          </div>
        </div>
      </SettingRow>

      <SettingRow
        label={
          <>
            {t.security.totp.label} {s?.totpEnabled ? <Badge tone="success">{t.security.totp.enabled}</Badge> : <Badge>{t.security.totp.disabled}</Badge>}
          </>
        }
        description={t.security.totp.description}
        help={t.security.totp.help}
      >
        <div className="grid gap-4">
          {feedback("totp")}
          {!s?.totpEnabled && !setup && (
            <div>
              <Button size="sm" variant="outline" onClick={() => run("totp", async () => setSetup(await settingsClient.beginTOTP({})), "")}>
                {t.security.totp.enable}
              </Button>
            </div>
          )}
          {setup && !s?.totpEnabled && (
            <>
              <div className="flex flex-wrap items-start gap-5">
                {/* 二维码固定黑底白字，深色模式下也能扫。 */}
                <div
                  className="size-36 shrink-0 rounded-md bg-white p-1.5 [&_svg]:size-full"
                  role="img"
                  aria-label={t.security.totp.qrCode}
                  dangerouslySetInnerHTML={{ __html: renderSVG(setup.uri, { border: 1 }) }}
                />
                <div className="grid min-w-0 flex-1 gap-3">
                  <p className="text-[13px] text-muted-foreground">{t.security.totp.scanHint}</p>
                  <CodeBlock copy>{setup.secret}</CodeBlock>
                </div>
              </div>
              <Field label={t.security.totp.code} htmlFor="totp-enable" hint={t.security.totp.codeHint}>
                <Input id="totp-enable" className="w-40 font-mono" placeholder="000000" value={code} onChange={(e) => setCode(e.target.value)} />
              </Field>
              <div className="flex gap-2">
                <Button
                  size="sm"
                  onClick={() =>
                    run(
                      "totp",
                      async () => {
                        await settingsClient.enableTOTP({ code });
                        setSetup(null);
                        setCode("");
                      },
                      t.security.totp.enabledMsg,
                    )
                  }
                >
                  {t.security.totp.confirmEnable}
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    setSetup(null);
                    setCode("");
                  }}
                >
                  {t.common.actions.cancel}
                </Button>
              </div>
            </>
          )}
          {s?.totpEnabled && (
            <div className="flex flex-wrap items-end gap-2">
              <Field label={t.security.totp.disableLabel} htmlFor="totp-disable" help={t.security.totp.disableHelp}>
                <Input
                  id="totp-disable"
                  type={/^\d{0,6}$/.test(code) ? "text" : "password"}
                  autoComplete="off"
                  className="h-8 w-48 font-mono"
                  placeholder={t.security.totp.disablePlaceholder}
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                />
              </Field>
              <Button
                size="sm"
                variant="destructive-outline"
                onClick={() =>
                  run(
                    "totp",
                    // 6 位数字按验证码，其余按登录密码。
                    () => settingsClient.disableTOTP(/^\d{6}$/.test(code.trim()) ? { code: code.trim() } : { password: code }),
                    t.security.totp.disabledMsg,
                  )
                }
              >
                {t.security.totp.disable}
              </Button>
            </div>
          )}
        </div>
      </SettingRow>

      <SettingRow label={t.security.entrance.label} description={t.security.entrance.description} help={t.security.entrance.help}>
        <div className="grid gap-3">
          {feedback("entrance")}
          {s?.entrance && <CodeBlock copy>{`${window.location.origin}/${s.entrance}`}</CodeBlock>}
          <div>
            {s?.entrance ? (
              <Button size="sm" variant="outline" onClick={() => run("entrance", () => settingsClient.setEntrance({ enabled: false }), t.security.entrance.disabledMsg)}>
                {t.security.entrance.disable}
              </Button>
            ) : (
              <Button size="sm" variant="outline" onClick={() => run("entrance", () => settingsClient.setEntrance({ enabled: true }), t.security.entrance.enabledMsg)}>
                {t.security.entrance.enable}
              </Button>
            )}
          </div>
        </div>
      </SettingRow>
    </SettingRows>
  );
}
