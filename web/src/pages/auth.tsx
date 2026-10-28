import { Code, ConnectError } from "@connectrpc/connect";
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Navigate, useNavigate } from "@tanstack/react-router";
import { authClient, errorMessage, setupClient } from "@/lib/api";
import { Alert, Button, Field, Input, Notice, Spinner, Waves } from "@/components/ui";
import { LanguageButton } from "@/components/language";
import { t } from "@/i18n";

// AuthShell：登录、初始化页面的外框。居中的窄卡片，上方是品牌标识。
function AuthShell({ title, description, children }: { title: string; description: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4 py-12">
      <div className="fixed top-4 right-4">
        <LanguageButton />
      </div>
      <div className="w-full max-w-sm">
        <div className="mb-6 flex flex-col items-center gap-2.5">
          <Waves className="h-5 w-12 text-wave" />
          <span className="text-base font-semibold tracking-tight">
            凪 <span className="font-normal text-muted-foreground">Nagi</span>{/* i18n-ignore */}
          </span>
        </div>
        <div className="rounded-lg border bg-card px-6 pt-6 pb-6 shadow-xs">
          <div className="mb-5">
            <h1 className="text-lg font-semibold tracking-tight">{title}</h1>
            <p className="mt-1 text-[13px] text-muted-foreground">{description}</p>
          </div>
          {children}
        </div>
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
    <AuthShell title={t.auth.setup.title} description={t.auth.setup.description}>
      {!token ? (
        <Notice tone="danger" title={t.auth.setup.missingToken}>
          {t.auth.setup.missingTokenHint}
        </Notice>
      ) : (
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            setLocalError("");
            if (password !== confirm) {
              setLocalError(t.auth.setup.passwordMismatch);
              return;
            }
            setup.mutate();
          }}
        >
          <Field label={t.auth.username} htmlFor="username">
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              placeholder="admin"
              autoFocus
              required
            />
          </Field>
          <Field label={t.auth.password} htmlFor="password" hint={t.auth.setup.passwordHint}>
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
          <Field label={t.auth.setup.confirmPassword} htmlFor="confirm">
            <Input
              id="confirm"
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
              required
            />
          </Field>
          {(localError || setup.error) && <Alert>{localError || errorMessage(setup.error)}</Alert>}
          <Button type="submit" className="mt-1 w-full" disabled={setup.isPending}>
            {setup.isPending && <Spinner />}
            {t.auth.setup.submit}
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
      if (err instanceof ConnectError && err.code === Code.FailedPrecondition) setNeedCode(true);
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries();
      navigate({ to: "/projects" });
    },
  });

  return (
    <AuthShell
      title={needCode ? t.auth.login.totpTitle : t.auth.login.title}
      description={needCode ? t.auth.login.totpDescription : t.auth.login.description}
    >
      {setupState.data && !setupState.data.initialized ? (
        <Notice tone="warning" title={t.auth.login.notInitialized}>
          {t.auth.login.notInitializedHint(<span className="font-mono">{t.auth.login.setupUrl}</span>)}
        </Notice>
      ) : (
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            login.mutate();
          }}
        >
          <Field label={t.auth.username} htmlFor="username">
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              autoFocus
              required
            />
          </Field>
          <Field label={t.auth.password} htmlFor="password">
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
            <Field label={t.auth.login.code} htmlFor="totp">
              <Input
                id="totp"
                inputMode="numeric"
                maxLength={6}
                value={totpCode}
                onChange={(e) => setTotpCode(e.target.value)}
                autoComplete="one-time-code"
                className="font-mono tracking-widest"
                placeholder="000000"
                autoFocus
              />
            </Field>
          )}
          {login.error && !(needCode && !totpCode) && <Alert>{errorMessage(login.error)}</Alert>}
          <Button type="submit" className="mt-1 w-full" disabled={login.isPending}>
            {login.isPending && <Spinner />}
            {needCode ? t.auth.login.verifySubmit : t.auth.login.submit}
          </Button>
        </form>
      )}
    </AuthShell>
  );
}
