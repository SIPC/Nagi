import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { authClient, errorMessage, settingsClient } from "@/lib/api";
import { Alert, Badge, Button, CodeBlock, Field, Input, Section } from "@/components/ui";

type Scope = "password" | "totp" | "entrance";

// 账号安全：面板主密钥、修改密码、两步验证、安全入口，各占一个区块。
export function SecurityCard() {
  const sec = useQuery({
    queryKey: ["security"],
    queryFn: () => settingsClient.getSecurity({}),
  });
  const [cur, setCur] = useState("");
  const [next, setNext] = useState("");
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(null);
  const [code, setCode] = useState("");
  // 提示和错误显示在触发它的区块里。
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
    <>
      {keyPath && (
        <Section
          title="面板主密钥"
          description="用于加密配置备份密钥、两步验证密钥和告警令牌。"
          help="请单独备份此文件。主密钥丢失后，即使在其他机器上恢复面板数据库，上述密钥也无法解密。"
        >
          <Field label="文件路径">
            <CodeBlock copy>{keyPath}</CodeBlock>
          </Field>
        </Section>
      )}

      <Section title="登录密码" help="忘记密码时，在面板所在机器上执行 nagi reset password。">
        <div className="grid gap-4">
          {feedback("password")}
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="当前密码" htmlFor="sec-cur">
              <Input id="sec-cur" type="password" value={cur} onChange={(e) => setCur(e.target.value)} />
            </Field>
            <Field label="新密码" htmlFor="sec-next" hint="至少 8 位">
              <Input id="sec-next" type="password" value={next} onChange={(e) => setNext(e.target.value)} />
            </Field>
          </div>
          <div className="flex justify-end">
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
      </Section>

      <Section
        title="两步验证"
        description="登录时额外校验验证器应用生成的 6 位验证码。"
        help="丢失验证器时，在面板所在机器上执行 nagi reset 2fa。"
        actions={s?.totpEnabled ? <Badge tone="success">已开启</Badge> : <Badge>未开启</Badge>}
      >
        <div className="grid gap-4">
          {feedback("totp")}
          {!s?.totpEnabled && !setup && (
            <div>
              <Button size="sm" onClick={() => run("totp", async () => setSetup(await settingsClient.beginTOTP({})), "")}>
                开启两步验证
              </Button>
            </div>
          )}
          {setup && !s?.totpEnabled && (
            <>
              <Field label="密钥" help="在验证器应用中手动添加此密钥，或使用下方地址生成二维码。">
                <CodeBlock copy>{setup.secret}</CodeBlock>
              </Field>
              <Field label="otpauth 地址">
                <CodeBlock copy>{setup.uri}</CodeBlock>
              </Field>
              <Field label="验证码" htmlFor="totp-enable" hint="验证器应用显示的 6 位数字">
                <Input id="totp-enable" className="w-40 font-mono" placeholder="000000" value={code} onChange={(e) => setCode(e.target.value)} />
              </Field>
              <div className="flex justify-end">
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
              </div>
            </>
          )}
          {s?.totpEnabled && (
            <div className="flex flex-wrap items-end gap-2">
              <Field label="验证码" htmlFor="totp-disable" help="关闭两步验证前需输入当前验证码。">
                <Input id="totp-disable" className="h-8 w-40 font-mono" placeholder="000000" value={code} onChange={(e) => setCode(e.target.value)} />
              </Field>
              <Button size="sm" variant="destructive-outline" onClick={() => run("totp", () => settingsClient.disableTOTP({ code }), "已关闭两步验证")}>
                关闭两步验证
              </Button>
            </div>
          )}
        </div>
      </Section>

      <Section
        title="安全入口"
        description="开启后，需先访问入口地址才能打开登录页。"
        help="忘记入口地址时，在面板所在机器上执行 nagi reset entrance。"
        actions={
          s?.entrance ? (
            <Button size="sm" variant="outline" onClick={() => run("entrance", () => settingsClient.setEntrance({ enabled: false }), "已关闭安全入口")}>
              关闭安全入口
            </Button>
          ) : (
            <Button size="sm" onClick={() => run("entrance", () => settingsClient.setEntrance({ enabled: true }), "已开启安全入口，请记录入口地址")}>
              开启安全入口
            </Button>
          )
        }
      >
        {(s?.entrance || msg?.scope === "entrance" || error?.scope === "entrance") && (
          <div className="grid gap-4">
            {feedback("entrance")}
            {s?.entrance && (
              <Field label="入口地址">
                <CodeBlock copy>{`${window.location.origin}/${s.entrance}`}</CodeBlock>
              </Field>
            )}
          </div>
        )}
      </Section>
    </>
  );
}
