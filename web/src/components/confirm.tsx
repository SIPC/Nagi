// 确认框：替代浏览器的 confirm / alert。第一句（到第一个问号）是标题，其余是后果说明，主按钮写具体的动作。
import { useEffect, useRef, useState } from "react";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button, Check, Field, Input } from "@/components/ui";
import { t } from "@/i18n";

type Options = {
  // 主按钮的文字，例如「重启」；不写时是「确认」。
  action?: string;
  danger?: boolean;
  // 附加的复选框，例如「同时删除容器目录」，默认不勾选。
  check?: string;
  // 勾选复选框后，要输入这段文字才能确认（数据无法恢复的操作）。
  typeToConfirm?: { label: string; value: string };
};

type Request = Options & {
  message: string;
  // 只有一个按钮的提示，相当于 alert。
  notice?: boolean;
  resolve: (r: { checked: boolean } | false) => void;
};

let show: ((r: Request) => void) | undefined;

// ask 显示确认框。取消时返回 false，确认时返回复选框的状态。
export function ask(message: string, options: Options = {}): Promise<{ checked: boolean } | false> {
  return new Promise((resolve) => {
    if (show) show({ ...options, message, resolve });
    else resolve(false);
  });
}

// confirmAction 显示确认框，返回是否确认。
export async function confirmAction(message: string, options: Options = {}): Promise<boolean> {
  return (await ask(message, options)) !== false;
}

// notify 显示只有一个按钮的提示。
export function notify(message: string): Promise<void> {
  return new Promise((resolve) => {
    if (show) show({ message, notice: true, resolve: () => resolve() });
    else resolve();
  });
}

// 标题是消息的第一句（含问号），其余是说明；没有问号时整段作为标题。
function splitMessage(message: string): [string, string] {
  const i = message.search(/[？?]/); // i18n-ignore
  if (i < 0 || i === message.length - 1) return [message, ""];
  return [message.slice(0, i + 1), message.slice(i + 1).trim()];
}

// ConfirmHost 放在页面最外层，同一时间显示一个确认框。焦点限制和归还、Esc 关闭由 AlertDialog 处理；点遮罩不关闭。
export function ConfirmHost() {
  const [req, setReq] = useState<Request>();
  const [checked, setChecked] = useState(false);
  const [typed, setTyped] = useState("");
  const safeButton = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    show = (r) => {
      setChecked(false);
      setTyped("");
      setReq(r);
    };
    return () => {
      show = undefined;
    };
  }, []);

  const finish = (result: { checked: boolean } | false) => {
    req?.resolve(result);
    setReq(undefined);
  };

  const [title, body] = req ? splitMessage(req.message) : ["", ""];
  const needType = !!req?.typeToConfirm && checked;
  const blocked = needType && typed !== req?.typeToConfirm?.value;

  return (
    <AlertDialog open={!!req} onOpenChange={(v) => !v && finish(false)}>
      <AlertDialogContent
        {...(body ? {} : { "aria-describedby": undefined })}
        // 默认焦点：危险操作在「取消」上，按回车不会误操作；其余在主按钮上。
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          safeButton.current?.focus();
        }}
      >
        <AlertDialogTitle>{title}</AlertDialogTitle>
        {body && <AlertDialogDescription>{body}</AlertDialogDescription>}
        {req?.check && (
          <Check checked={checked} onChange={setChecked}>
            {req.check}
          </Check>
        )}
        {needType && req?.typeToConfirm && (
          <Field label={req.typeToConfirm.label} htmlFor="confirm-type">
            <Input id="confirm-type" value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" autoFocus />
          </Field>
        )}
        <div className="flex justify-end gap-2">
          {req?.notice ? (
            <AlertDialogAction asChild>
              <Button ref={safeButton} readOnlyOk size="sm" onClick={() => finish({ checked: false })}>
                {t.common.actions.confirm}
              </Button>
            </AlertDialogAction>
          ) : (
            <>
              <AlertDialogCancel asChild>
                <Button ref={req?.danger ? safeButton : undefined} readOnlyOk size="sm" variant="ghost">
                  {t.common.actions.cancel}
                </Button>
              </AlertDialogCancel>
              <AlertDialogAction asChild>
                <Button
                  ref={req?.danger ? undefined : safeButton}
                  readOnlyOk
                  size="sm"
                  variant={req?.danger ? "destructive" : "default"}
                  disabled={blocked}
                  onClick={() => finish({ checked })}
                >
                  {req?.action ?? t.common.actions.confirm}
                </Button>
              </AlertDialogAction>
            </>
          )}
        </div>
      </AlertDialogContent>
    </AlertDialog>
  );
}
