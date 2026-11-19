// 手机确认（见 docs/android.md#手机确认）：发起后显示两位数字，等成员在手机 App 上照着输入并确认。
// 高权限确认（elevate.tsx）和登录的第二步（pages/auth.tsx）共用。
import { useEffect, useRef, useState } from "react";
import { Alert, Button, Spinner } from "@/components/ui";
import type { PhoneApproval } from "@/gen/nagi/v1/api_pb";
import { t } from "@/i18n";
import { errorMessage } from "@/lib/api";

type Status = "pending" | "approved" | "denied" | "expired";

// begin 发起请求；poll 查结果（approved 时调用方已经拿到会话或高权限确认）；onApproved 确认后调用；onBack 返回上一步。
export function PhoneApprovalWait({
  begin,
  poll,
  onApproved,
  onBack,
  backLabel,
}: {
  begin: () => Promise<PhoneApproval>;
  poll: (a: PhoneApproval) => Promise<string>;
  onApproved: () => void;
  onBack: () => void;
  backLabel: string;
}) {
  const m = t.common.phone;
  const [approval, setApproval] = useState<PhoneApproval>();
  const [status, setStatus] = useState<Status>("pending");
  const [error, setError] = useState("");
  const [round, setRound] = useState(0);
  // 回调每次渲染都是新的函数，放在 ref 里，免得重新发起。
  const fns = useRef({ begin, poll, onApproved });
  fns.current = { begin, poll, onApproved };
  // 同一轮只发起一次：开发模式下 effect 会连着跑两遍，第二遍接着用第一遍发起的请求（面板不接受同时发起两个）。
  const started = useRef<{ round: number; promise: Promise<PhoneApproval> }>(undefined);

  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    setApproval(undefined);
    setStatus("pending");
    setError("");
    (async () => {
      let a: PhoneApproval;
      try {
        if (started.current?.round !== round) started.current = { round, promise: fns.current.begin() };
        a = await started.current.promise;
      } catch (err) {
        if (!stopped) setError(errorMessage(err));
        return;
      }
      if (stopped) return;
      setApproval(a);
      const tick = async () => {
        if (stopped) return;
        try {
          const s = (await fns.current.poll(a)) as Status;
          if (stopped) return;
          if (s === "approved") {
            setStatus(s);
            fns.current.onApproved();
            return;
          }
          if (s === "denied" || s === "expired") {
            setStatus(s);
            return;
          }
        } catch (err) {
          // 请求已经不在了（过期、面板重启过）：按过期处理；别的错误显示出来。
          if (!stopped) {
            setError(errorMessage(err));
            setStatus("expired");
          }
          return;
        }
        timer = setTimeout(tick, 1500);
      };
      timer = setTimeout(tick, 1500);
    })();
    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
    };
  }, [round]);

  const ended = status === "denied" || status === "expired";
  return (
    <div className="grid gap-4">
      {approval && !ended && (
        <div className="grid justify-items-center gap-2 py-2">
          <span className="font-mono text-4xl leading-none font-medium tracking-widest text-foreground tabular-nums" aria-label={m.codeLabel}>
            {approval.code}
          </span>
          <span className="flex items-center gap-2 text-sm leading-5 text-muted-foreground">
            <Spinner />
            {m.waiting}
          </span>
        </div>
      )}
      {!approval && !error && (
        <div className="flex justify-center py-4">
          <Spinner />
        </div>
      )}
      {status === "denied" && <Alert>{m.denied}</Alert>}
      {status === "expired" && !error && <Alert>{m.expired}</Alert>}
      {error && <Alert>{error}</Alert>}
      <div className="grid gap-2">
        {(ended || error) && (
          <Button className="w-full" readOnlyOk onClick={() => setRound((n) => n + 1)}>
            {m.retry}
          </Button>
        )}
        <Button variant="ghost" className="w-full" readOnlyOk onClick={onBack}>
          {backLabel}
        </Button>
      </div>
    </div>
  );
}
