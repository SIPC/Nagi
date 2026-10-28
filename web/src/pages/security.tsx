import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { renderSVG } from "uqr";
import { authClient, errorMessage, settingsClient } from "@/lib/api";
import { Alert, Badge, Button, CodeBlock, Field, Input, SettingRow, SettingRows } from "@/components/ui";

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
          label="面板主密钥"
          description="用于加密配置备份密钥、两步验证密钥和告警令牌，请单独备份。"
          help="主密钥丢失后，即使在其他机器上恢复面板数据库，上述密钥也无法解密。"
        >
          <CodeBlock copy>{keyPath}</CodeBlock>
        </SettingRow>
      )}

      <SettingRow label="登录密码" description="修改后其他会话将退出登录。" help="忘记密码时，在面板所在机器上执行 nagi reset password。">
        <div className="grid max-w-sm gap-3">
          {feedback("password")}
          <Field label="当前密码" htmlFor="sec-cur">
            <Input id="sec-cur" type="password" value={cur} onChange={(e) => setCur(e.target.value)} />
          </Field>
          <Field label="新密码" htmlFor="sec-next" hint="至少 8 个字符">
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
                  "已修改密码，其他会话已退出登录",
                )
              }
            >
              修改密码
            </Button>
          </div>
        </div>
      </SettingRow>

      <SettingRow
        label={
          <>
            两步验证 {s?.totpEnabled ? <Badge tone="success">已开启</Badge> : <Badge>未开启</Badge>}
          </>
        }
        description="登录时额外校验验证器应用生成的 6 位验证码。"
        help="丢失验证器时，在面板所在机器上执行 nagi reset 2fa。"
      >
        <div className="grid gap-4">
          {feedback("totp")}
          {!s?.totpEnabled && !setup && (
            <div>
              <Button size="sm" variant="outline" onClick={() => run("totp", async () => setSetup(await settingsClient.beginTOTP({})), "")}>
                开启两步验证
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
                  aria-label="两步验证二维码"
                  dangerouslySetInnerHTML={{ __html: renderSVG(setup.uri, { border: 1 }) }}
                />
                <div className="grid min-w-0 flex-1 gap-3">
                  <p className="text-[13px] text-muted-foreground">使用验证器应用（如 Google Authenticator、1Password）扫描二维码；无法扫码时可手动输入密钥。</p>
                  <CodeBlock copy>{setup.secret}</CodeBlock>
                </div>
              </div>
              <Field label="验证码" htmlFor="totp-enable" hint="6 位数字">
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
                      "已开启两步验证，其他会话已退出登录",
                    )
                  }
                >
                  确认开启
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    setSetup(null);
                    setCode("");
                  }}
                >
                  取消
                </Button>
              </div>
            </>
          )}
          {s?.totpEnabled && (
            <div className="flex flex-wrap items-end gap-2">
              <Field label="验证码或登录密码" htmlFor="totp-disable" help="关闭两步验证前需验证身份：输入验证器应用当前的 6 位验证码；验证器丢失时输入登录密码。">
                <Input
                  id="totp-disable"
                  type={/^\d{0,6}$/.test(code) ? "text" : "password"}
                  autoComplete="off"
                  className="h-8 w-48 font-mono"
                  placeholder="000000 或密码"
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
                    "已关闭两步验证",
                  )
                }
              >
                关闭两步验证
              </Button>
            </div>
          )}
        </div>
      </SettingRow>

      <SettingRow label="安全入口" description="开启后须通过入口地址访问登录页。" help="忘记入口地址时，在面板所在机器上执行 nagi reset entrance。">
        <div className="grid gap-3">
          {feedback("entrance")}
          {s?.entrance && <CodeBlock copy>{`${window.location.origin}/${s.entrance}`}</CodeBlock>}
          <div>
            {s?.entrance ? (
              <Button size="sm" variant="outline" onClick={() => run("entrance", () => settingsClient.setEntrance({ enabled: false }), "已关闭安全入口")}>
                关闭安全入口
              </Button>
            ) : (
              <Button size="sm" variant="outline" onClick={() => run("entrance", () => settingsClient.setEntrance({ enabled: true }), "已开启安全入口，请记录入口地址")}>
                开启安全入口
              </Button>
            )}
          </div>
        </div>
      </SettingRow>
    </SettingRows>
  );
}
