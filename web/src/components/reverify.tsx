// 重新验证身份（见 docs/panel.md#会话）：面板发现这个会话的设备、网络或会话密钥对不上时，所有请求都返回
// 「请重新验证身份」。这里弹框：开了两步验证的输验证码，没开的输密码。验证通过后会话继续，不用重新登录；
// 连续 3 次失败面板退出这个会话。
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Alert, Button, Dialog, DialogActions, Field, Input, Spinner } from "@/components/ui";
import { t } from "@/i18n";
import { authClient, errorMessage, isUnauthenticated, setSessionHandlers } from "@/lib/api";
import { ensureSessionKey } from "@/lib/session";

type State = { reason: string; method: string };

let current: State | undefined;
let listeners: ((s: State | undefined) => void)[] = [];
let waiters: ((ok: boolean) => void)[] = [];

function setState(s: State | undefined) {
  current = s;
  listeners.forEach((l) => l(s));
}

setSessionHandlers({ reverify: (reason, method) => setState({ reason, method }) });

// waitReverify：等到重新验证完成（true）或退出登录（false）。
export function waitReverify(): Promise<boolean> {
  if (!current) return Promise.resolve(true);
  return new Promise((resolve) => waiters.push(resolve));
}

function done(ok: boolean) {
  setState(undefined);
  const w = waiters;
  waiters = [];
  w.forEach((f) => f(ok));
}

export function ReverifyHost() {
  const queryClient = useQueryClient();
  const [state, set] = useState<State | undefined>(current);
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const m = t.account.reverify;

  useEffect(() => {
    const l = (s: State | undefined) => {
      set(s);
      setValue("");
      setError("");
    };
    listeners.push(l);
    return () => {
      listeners = listeners.filter((x) => x !== l);
    };
  }, []);

  const logout = async () => {
    try {
      await authClient.logout({});
    } finally {
      done(false);
      window.location.assign("/login");
    }
  };

  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await authClient.reverify(state?.method === "totp" ? { totpCode: value.trim() } : { password: value });
      await ensureSessionKey();
      done(true);
      queryClient.invalidateQueries();
    } catch (err) {
      if (isUnauthenticated(err)) {
        // 失败次数过多，会话已退出。
        done(false);
        window.location.assign("/login");
        return;
      }
      setError(errorMessage(err));
      setValue("");
    } finally {
      setBusy(false);
    }
  };

  const reasons = m.reasons as Record<string, string>;
  return (
    <Dialog open={!!state} onClose={() => {}} dismissible={false} title={m.title} description={(state && reasons[state.reason]) || m.description}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        {state?.method === "totp" ? (
          <Field label={m.code} htmlFor="reverify-value">
            <Input id="reverify-value" name="totp" value={value} onChange={(e) => setValue(e.target.value)} inputMode="numeric" pattern="[0-9]*" maxLength={6} autoComplete="one-time-code" autoFocus required />
          </Field>
        ) : (
          <Field label={m.password} htmlFor="reverify-value">
            <Input id="reverify-value" type="password" value={value} onChange={(e) => setValue(e.target.value)} autoComplete="current-password" autoFocus required />
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          <Button variant="outline" readOnlyOk onClick={logout}>
            {m.logout}
          </Button>
          <Button type="submit" readOnlyOk disabled={busy}>
            {busy && <Spinner />}
            {m.submit}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
