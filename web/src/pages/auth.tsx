import { Code, ConnectError } from "@connectrpc/connect";
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Navigate, useNavigate } from "@tanstack/react-router";
import { authClient, errorMessage, setupClient } from "@/lib/api";
import { Button, Field, Input, Notice, QueryError, Spinner, Waves } from "@/components/ui";
import { LanguageButton } from "@/components/language";
import { t } from "@/i18n";

// returnToKey：OAuth 授权页在没登录时记下自己的地址，登录后回到那里（见 pages/tokens.tsx）。
export const returnToKey = "nagi.returnTo";

// AuthShell：登录、初始化页面的外框。居中 360 宽，没有卡片：品牌、标题、内容。
// 只有一条提示（未初始化、连不上面板）时不写标题。
export function AuthShell({ title, description, children }: { title?: string; description?: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4 py-8">
      <div className="fixed top-4 right-4">
        <LanguageButton />
      </div>
      <main className="grid w-full max-w-[320px] gap-6">
        <div className="grid justify-items-start gap-3">
          <Waves className="h-6 w-[42px] text-wave" />
          <span className="sr-only">Nagi</span>
          {title && (
            <div className="grid gap-1">
              <h1 className="text-2xl leading-8 font-semibold tracking-tight">{title}</h1>
              {description && <p className="text-muted-foreground">{description}</p>}
            </div>
          )}
        </div>
        {children}
      </main>
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
      navigate({ to: "/" });
    },
  });

  // 已经初始化：去项目页（刚初始化完已经登录；没登录的话项目页会再转去登录页）。
  if (state.data?.initialized) return <Navigate to="/projects" />;

  if (!token)
    return (
      <AuthShell>
        <Notice tone="danger" title={t.auth.setup.missingToken}>
          {t.auth.setup.missingTokenHint}
        </Notice>
      </AuthShell>
    );

  if (state.error && !state.data)
    return (
      <AuthShell>
        <QueryError error={errorMessage(state.error)} onRetry={() => state.refetch()} />
      </AuthShell>
    );

  const error = localError || (setup.error ? errorMessage(setup.error) : undefined);

  return (
    <AuthShell title={t.auth.setup.title} description={t.auth.setup.description}>
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
          <Input id="username" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" placeholder="admin" autoFocus required />
        </Field>
        <Field label={t.auth.password} htmlFor="password">
          <Input id="password" type="password" placeholder={t.auth.setup.passwordHint} value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" required minLength={8} />
        </Field>
        <Field label={t.auth.setup.confirmPassword} htmlFor="confirm" error={error}>
          <Input id="confirm" type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
        </Field>
        <Button type="submit" className="w-full" disabled={setup.isPending}>
          {setup.isPending && <Spinner />}
          {t.auth.setup.submit}
        </Button>
      </form>
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
      // 从 OAuth 授权页过来的，登录后回去（见 pages/tokens.tsx）。
      let back = "";
      try {
        back = sessionStorage.getItem(returnToKey) ?? "";
        sessionStorage.removeItem(returnToKey);
      } catch {
        // 读不了就回首页。
      }
      if (back.startsWith("/oauth/authorize?")) window.location.assign(back);
      else navigate({ to: "/" });
    },
  });

  if (setupState.error && !setupState.data)
    return (
      <AuthShell>
        <QueryError error={errorMessage(setupState.error)} onRetry={() => setupState.refetch()} />
      </AuthShell>
    );

  if (setupState.data && !setupState.data.initialized)
    return (
      <AuthShell>
        <Notice tone="warning" title={t.auth.login.notInitialized}>
          {t.auth.login.notInitializedHint(t.auth.login.setupUrl)}
        </Notice>
      </AuthShell>
    );

  // 刚进入两步验证时的「需要验证码」不算错误，还没有填。
  const error = login.error && !(needCode && !totpCode) ? errorMessage(login.error) : undefined;
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    login.mutate();
  };

  if (needCode)
    return (
      <AuthShell title={t.auth.login.totpTitle} description={t.auth.login.totpDescription}>
        <form className="grid gap-4" onSubmit={submit}>
          <Field label={t.auth.login.code} htmlFor="totp" error={error}>
            <Input
              id="totp"
              inputMode="numeric"
              maxLength={6}
              value={totpCode}
              onChange={(e) => {
                setTotpCode(e.target.value);
                // 改了验证码，上一次的结果就不算了。
                if (login.error) login.reset();
              }}
              autoComplete="one-time-code"
              placeholder="000000"
              autoFocus
            />
          </Field>
          <div className="grid gap-2">
            <Button type="submit" className="w-full" disabled={login.isPending}>
              {login.isPending && <Spinner />}
              {t.auth.login.verifySubmit}
            </Button>
            <Button
              variant="ghost"
              className="w-full"
              disabled={login.isPending}
              onClick={() => {
                setNeedCode(false);
                setTotpCode("");
                login.reset();
              }}
            >
              {t.auth.login.back}
            </Button>
          </div>
        </form>
      </AuthShell>
    );

  return (
    <AuthShell title={t.auth.login.title}>
      <form className="grid gap-4" onSubmit={submit}>
        <Field label={t.auth.username} htmlFor="username">
          <Input id="username" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" autoFocus required />
        </Field>
        <Field label={t.auth.password} htmlFor="password" error={error}>
          <Input id="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
        </Field>
        <Button type="submit" className="w-full" disabled={login.isPending}>
          {login.isPending && <Spinner />}
          {t.auth.login.submit}
        </Button>
      </form>
    </AuthShell>
  );
}
