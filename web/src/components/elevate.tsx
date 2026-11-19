// 高权限确认：打开节点终端、访问节点文件前再验证一次身份（见 docs/node-access.md#权限）。
// 记在服务端的会话上，这里只负责问。有效期内不再询问。
import { Code, ConnectError } from "@connectrpc/connect";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { PhoneApprovalWait } from "@/components/phone-approval";
import { Alert, Button, Dialog, DialogActions, Field, Input, Spinner, textLink } from "@/components/ui";
import { t } from "@/i18n";
import { authClient, errorMessage, toDate } from "@/lib/api";

// suggestTotp：没开两步验证时在密码旁建议开启。正在开启两步验证时不提示。
// phone：可以改在手机 App 上确认（添加过手机，而且这个操作认手机的确认）。
type Request = { totp: boolean; phone: boolean; description?: string; suggestTotp: boolean; resolve: (ok: boolean) => void };

let show: ((r: Request) => void) | undefined;

// requireElevation 确认过了就直接返回 true，否则弹框。取消时返回 false。description 换掉对话框的说明（默认是节点权限的）。
// passwordOnly：给自己留登录凭据的操作（生成令牌、添加通行密钥、开启两步验证、添加手机）只认密码或验证码，
// 手机上做的确认不算，也不给「在手机上确认」（见 docs/android.md#高权限确认和重新验证）。
export async function requireElevation(description?: string, options: { suggestTotp?: boolean; passwordOnly?: boolean } = {}): Promise<boolean> {
  const me = await authClient.me({});
  const until = toDate(me.elevatedUntil);
  if (until && until.getTime() > Date.now() + 5000 && !(options.passwordOnly && me.elevatedBy === "app")) return true;
  return new Promise((resolve) => {
    if (show) show({ totp: me.totpEnabled, phone: me.hasPhone && !options.passwordOnly, description, suggestTotp: options.suggestTotp ?? true, resolve });
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
  const [onPhone, setOnPhone] = useState(false);
  const m = t.nodeAccess.elevate;

  useEffect(() => {
    show = (r) => {
      setValue("");
      setError("");
      setOnPhone(false);
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
    <Dialog
      open={!!req}
      onClose={() => finish(false)}
      title={m.title}
      description={
        <>
          {req?.description ?? m.description}
          {/* 没开两步验证时的建议放在说明里，字段下面只写格式。 */}
          {!req?.totp && req?.suggestTotp && (
            <>
              {" "}
              {m.enableTotp}{" "}
              <Link to="/settings" search={{ tab: "security" }} className={textLink} onClick={() => finish(false)}>
                {m.securityLink}
              </Link>
            </>
          )}
        </>
      }
    >
      {onPhone ? (
        <PhoneApprovalWait
          begin={() => authClient.beginPhoneApproval({})}
          poll={async (a) => (await authClient.getPhoneApproval({ id: a.id })).status}
          onApproved={() => {
            queryClient.invalidateQueries({ queryKey: ["me"] });
            finish(true);
          }}
          onBack={() => setOnPhone(false)}
          backLabel={req?.totp ? t.common.phone.useCode : t.common.phone.usePassword}
        />
      ) : (
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        {req?.totp ? (
          <Field label={m.code} htmlFor="elevate-value">
            <Input id="elevate-value" name="totp" value={value} onChange={(e) => setValue(e.target.value)} inputMode="numeric" pattern="[0-9]*" maxLength={6} autoComplete="one-time-code" autoFocus required />
          </Field>
        ) : (
          <Field label={m.password} htmlFor="elevate-value">
            <Input id="elevate-value" type="password" value={value} onChange={(e) => setValue(e.target.value)} autoComplete="current-password" autoFocus required />
          </Field>
        )}
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          {req?.phone && (
            <Button variant="ghost" className="mr-auto" readOnlyOk onClick={() => setOnPhone(true)}>
              {t.common.phone.use}
            </Button>
          )}
          <Button variant="outline" readOnlyOk onClick={() => finish(false)}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy}>
            {busy && <Spinner />}
            {m.submit}
          </Button>
        </DialogActions>
      </form>
      )}
    </Dialog>
  );
}
