// 高权限确认：打开节点终端、访问节点文件前再验证一次身份（见 docs/node-access.md#权限）。
// 记在服务端的会话上，这里只负责问。有效期内不再询问。
import { Code, ConnectError } from "@connectrpc/connect";
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Alert, Button, Dialog, DialogActions, Field, Input, Spinner } from "@/components/ui";
import { t } from "@/i18n";
import { authClient, errorMessage, toDate } from "@/lib/api";

type Request = { totp: boolean; description?: string; resolve: (ok: boolean) => void };

let show: ((r: Request) => void) | undefined;

// requireElevation 确认过了就直接返回 true，否则弹框。取消时返回 false。description 换掉对话框的说明（默认是节点权限的）。
export async function requireElevation(description?: string): Promise<boolean> {
  const me = await authClient.me({});
  const until = toDate(me.elevatedUntil);
  if (until && until.getTime() > Date.now() + 5000) return true;
  return new Promise((resolve) => {
    if (show) show({ totp: me.totpEnabled, description, resolve });
    else resolve(false);
  });
}

// isElevationRequired：接口因为确认过期而拒绝。
export function isElevationRequired(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.PermissionDenied && err.metadata.get("X-Nagi-Elevation") === "required";
}

export function ElevateHost() {
  const queryClient = useQueryClient();
  const [req, setReq] = useState<Request>();
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const m = t.nodeAccess.elevate;

  useEffect(() => {
    show = (r) => {
      setValue("");
      setError("");
      setReq(r);
    };
    return () => {
      show = undefined;
    };
  }, []);

  const finish = (ok: boolean) => {
    req?.resolve(ok);
    setReq(undefined);
  };

  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await authClient.elevate(req?.totp ? { totpCode: value.trim() } : { password: value });
      queryClient.invalidateQueries({ queryKey: ["me"] });
      finish(true);
    } catch (err) {
      setError(errorMessage(err));
      setValue("");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={!!req} onClose={() => finish(false)} title={m.title} description={req?.description ?? m.description}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        {req?.totp ? (
          <Field label={m.code} htmlFor="elevate-value">
            <Input id="elevate-value" value={value} onChange={(e) => setValue(e.target.value)} inputMode="numeric" autoComplete="one-time-code" autoFocus required />
          </Field>
        ) : (
          <Field label={m.password} htmlFor="elevate-value" help={m.enableTotp}>
            <Input id="elevate-value" type="password" value={value} onChange={(e) => setValue(e.target.value)} autoComplete="current-password" autoFocus required />
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          <Button variant="outline" readOnlyOk onClick={() => finish(false)}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy}>
            {busy && <Spinner />}
            {m.submit}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
