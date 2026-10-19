import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { authClient, errorMessage, settingsClient } from "@/lib/api";
import {
  Alert,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Input,
} from "@/components/ui";

// 账号安全（临时测试界面）：修改密码、两步验证、安全入口。
export function SecurityCard() {
  const sec = useQuery({
    queryKey: ["security"],
    queryFn: () => settingsClient.getSecurity({}),
  });
  const [cur, setCur] = useState("");
  const [next, setNext] = useState("");
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(
    null,
  );
  const [code, setCode] = useState("");
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");

  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setError("");
    setMsg("");
    try {
      await fn();
      setMsg(ok);
      sec.refetch();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const s = sec.data;
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsClient.getSettings({}) });
  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>账号安全</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4 text-sm">
        {error && <Alert>{error}</Alert>}
        {msg && <div className="text-muted-foreground">{msg}</div>}
        {settings.data?.settings?.masterKeyPath && (
          <p className="text-muted-foreground">
            面板主密钥：<span className="font-mono">{settings.data.settings.masterKeyPath}</span>
            。配置备份里的密钥、两步验证密钥、告警令牌都用它加密，请单独备份这个文件；丢了它，换一台机器恢复面板数据库也解不开这些密钥。
          </p>
        )}
        <div className="flex flex-wrap items-center gap-2">
          <span className="w-24">修改密码</span>
          <Input
            className="w-44"
            type="password"
            placeholder="当前密码"
            value={cur}
            onChange={(e) => setCur(e.target.value)}
          />
          <Input
            className="w-44"
            type="password"
            placeholder="新密码（至少 8 位）"
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
          <Button
            size="sm"
            variant="outline"
            onClick={() =>
              run(
                () =>
                  authClient.changePassword({
                    current: cur,
                    newPassword: next,
                  }),
                "密码已修改，其他地方的登录已失效",
              )
            }
          >
            修改
          </Button>
        </div>
        <div className="grid gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="w-24">两步验证</span>
            <span>{s?.totpEnabled ? "已开启" : "未开启"}</span>
            {!s?.totpEnabled && !setup && (
              <Button
                size="sm"
                variant="outline"
                onClick={() =>
                  run(
                    async () => setSetup(await settingsClient.beginTOTP({})),
                    "",
                  )
                }
              >
                开启
              </Button>
            )}
            {s?.totpEnabled && (
              <>
                <Input
                  className="w-32 font-mono"
                  placeholder="验证码"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                />
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() =>
                    run(
                      () => settingsClient.disableTOTP({ code }),
                      "已关闭两步验证",
                    )
                  }
                >
                  关闭
                </Button>
              </>
            )}
          </div>
          {setup && !s?.totpEnabled && (
            <div className="grid gap-2 rounded border p-3 text-xs">
              <div>
                在验证器 App
                里手动添加这个密钥（或用这个地址生成二维码），再输入它显示的 6
                位数字：
              </div>
              <code className="break-all font-mono">{setup.secret}</code>
              <code className="break-all font-mono text-muted-foreground">
                {setup.uri}
              </code>
              <div className="flex gap-2">
                <Input
                  className="w-32 font-mono"
                  placeholder="000000"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                />
                <Button
                  size="sm"
                  onClick={() =>
                    run(async () => {
                      await settingsClient.enableTOTP({ code });
                      setSetup(null);
                      setCode("");
                    }, "已开启两步验证，其他地方的登录都已退出")
                  }
                >
                  确认开启
                </Button>
              </div>
            </div>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="w-24">安全入口</span>
          {s?.entrance ? (
            <>
              <code className="font-mono">
                {window.location.origin}/{s.entrance}
              </code>
              <Button
                size="sm"
                variant="outline"
                onClick={() =>
                  run(
                    () => settingsClient.setEntrance({ enabled: false }),
                    "已关闭安全入口",
                  )
                }
              >
                关闭
              </Button>
            </>
          ) : (
            <Button
              size="sm"
              variant="outline"
              onClick={() =>
                run(
                  () => settingsClient.setEntrance({ enabled: true }),
                  "已开启：请记下上面的地址，以后要先打开它才能看到登录页",
                )
              }
            >
              开启
            </Button>
          )}
        </div>
        <div className="text-xs text-muted-foreground">
          忘记密码、丢了验证器或者忘了入口：在面板机器上执行 nagi reset password
          / 2fa / entrance。
        </div>
      </CardContent>
    </Card>
  );
}
