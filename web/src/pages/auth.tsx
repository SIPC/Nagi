import { Code, ConnectError } from "@connectrpc/connect";
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Navigate, useNavigate } from "@tanstack/react-router";
import { authClient, errorMessage, setupClient } from "@/lib/api";
import {
  Alert,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  Field,
  Input,
  Spinner,
  Waves,
} from "@/components/ui";

function AuthShell({
  title,
  description,
  children,
}: {
  title: string;
  description: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-h-screen items-center justify-center px-4 py-12">
      <div className="w-full max-w-sm">
        <div className="mb-8 flex flex-col items-center gap-2 text-center">
          <Waves className="h-6 w-14 text-wave" />
          <span className="text-lg font-semibold tracking-wide">
            凪 <span className="text-muted-foreground">Nagi</span>
          </span>
        </div>
        <Card>
          <CardHeader>
            <CardTitle>{title}</CardTitle>
            <CardDescription>{description}</CardDescription>
          </CardHeader>
          <CardContent>{children}</CardContent>
        </Card>
      </div>
    </div>
  );
}

export function SetupPage({ token }: { token: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const state = useQuery({
    queryKey: ["setup-state"],
    queryFn: () => setupClient.getSetupState({}),
  });
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [localError, setLocalError] = useState("");

  const setup = useMutation({
    mutationFn: () => setupClient.setup({ token, username, password }),
    onSuccess: async () => {
      await queryClient.invalidateQueries();
      navigate({ to: "/projects" });
    },
  });

  // 已经初始化：去项目页（刚初始化完已经登录；没登录的话项目页会再转去登录页）。
  if (state.data?.initialized) return <Navigate to="/projects" />;

  return (
    <AuthShell
      title="初始化面板"
      description="设置管理员账号。之后用它登录面板。"
    >
      {!token ? (
        <Alert>
          缺少初始化令牌。请使用面板启动日志里打印的初始化链接打开本页。
        </Alert>
      ) : (
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            setLocalError("");
            if (password !== confirm) {
              setLocalError("两次输入的密码不一致");
              return;
            }
            setup.mutate();
          }}
        >
          <Field label="用户名" htmlFor="username">
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              placeholder="例如 admin"
              required
            />
          </Field>
          <Field label="密码" htmlFor="password" hint="至少 8 个字符">
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
              required
              minLength={8}
            />
          </Field>
          <Field label="确认密码" htmlFor="confirm">
            <Input
              id="confirm"
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
              required
            />
          </Field>
          {(localError || setup.error) && (
            <Alert>{localError || errorMessage(setup.error)}</Alert>
          )}
          <Button type="submit" disabled={setup.isPending}>
            {setup.isPending && <Spinner />}
            完成初始化
          </Button>
        </form>
      )}
    </AuthShell>
  );
}

export function LoginPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [totpCode, setTotpCode] = useState("");
  const [needCode, setNeedCode] = useState(false);
  const setupState = useQuery({ queryKey: ["setup-state"], queryFn: () => setupClient.getSetupState({}) });

  const login = useMutation({
    mutationFn: () => authClient.login({ username, password, totpCode }),
    onError: (err) => {
      // 密码正确但开启了两步验证：显示验证码输入框。
      if (err instanceof ConnectError && err.code === Code.FailedPrecondition)
        setNeedCode(true);
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries();
      navigate({ to: "/projects" });
    },
  });

  return (
    <AuthShell title="登录" description="登录到 Nagi 面板。">
      {setupState.data && !setupState.data.initialized ? (
        <Alert>
          面板还没有初始化，没有可以登录的账号。请打开面板启动日志里打印的初始化链接（形如 https://地址:端口/setup?token=…），设置管理员账号。
        </Alert>
      ) : (
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          login.mutate();
        }}
      >
        <Field label="用户名" htmlFor="username">
          <Input
            id="username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            autoFocus
            required
          />
        </Field>
        <Field label="密码" htmlFor="password">
          <Input
            id="password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            required
          />
        </Field>
        {needCode && (
          <Field label="两步验证码" htmlFor="totp">
            <Input
              id="totp"
              inputMode="numeric"
              maxLength={6}
              value={totpCode}
              onChange={(e) => setTotpCode(e.target.value)}
              autoComplete="one-time-code"
              autoFocus
            />
          </Field>
        )}
        {login.error && !(needCode && !totpCode) && (
          <Alert>{errorMessage(login.error)}</Alert>
        )}
        <Button type="submit" disabled={login.isPending}>
          {login.isPending && <Spinner />}
          登录
        </Button>
      </form>
      )}
    </AuthShell>
  );
}
