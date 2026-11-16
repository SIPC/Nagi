import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import {
  AuthService,
  type ListNodesResponse,
  NodeService,
  ProjectService,
  SettingsService,
  SetupService,
} from "@/gen/nagi/v1/api_pb";
import { locale, t } from "@/i18n";

// 浏览器与面板同源，cookie 会随请求自动带上。
// Accept-Language 换成界面当前的语言（可能和浏览器的不同），面板按它返回错误信息。
// 会话加固（见 docs/panel.md#会话）：响应头里有挑战时交给 lib/session 签名换发令牌；
// 要求重新验证时交给 components/reverify 弹框。用回调避免循环引用。
type SessionHandlers = { challenge?: (c: string) => void; reverify?: (reason: string, method: string) => void };
const sessionHandlers: SessionHandlers = {};
export function setSessionHandlers(h: SessionHandlers) {
  Object.assign(sessionHandlers, h);
}

const challengeHeader = "Nagi-Session-Challenge";

const transport = createConnectTransport({
  baseUrl: window.location.origin,
  interceptors: [
    (next) => async (req) => {
      req.header.set("Accept-Language", locale);
      try {
        const res = await next(req);
        const c = res.header.get(challengeHeader);
        if (c) sessionHandlers.challenge?.(c);
        return res;
      } catch (err) {
        if (err instanceof ConnectError) {
          const c = err.metadata.get(challengeHeader);
          if (c) sessionHandlers.challenge?.(c);
          const reason = reverifyReason(err);
          if (reason) sessionHandlers.reverify?.(reason, err.metadata.get("Nagi-Reverify-Method") ?? "password");
        }
        throw err;
      }
    },
  ],
});

// reverifyReason：面板要求这个会话重新验证身份时的原因，不是时返回空。
export function reverifyReason(err: unknown): string {
  return err instanceof ConnectError && err.code === Code.Unauthenticated ? (err.metadata.get("Nagi-Reverify") ?? "") : "";
}

export const setupClient = createClient(SetupService, transport);
export const authClient = createClient(AuthService, transport);
export const nodeClient = createClient(NodeService, transport);
export const projectClient = createClient(ProjectService, transport);
export const settingsClient = createClient(SettingsService, transport);

// knownNodes：节点列表加上访问范围外、但项目里的容器用到的节点（只有名称和是否在线，见 docs/panel.md#访问范围）。
// 查节点名称和在线状态用；列出、选择节点只用 nodes。
export const knownNodes = (res: ListNodesResponse | undefined) => (res ? [...res.nodes, ...res.otherNodes] : []);

// errorMessage 返回给人看的错误信息。面板返回的错误原样显示；连不上面板、接口不存在这类没有说明的错误换成一句话。
export function errorMessage(err: unknown): string {
  if (err instanceof ConnectError) {
    const m = err.rawMessage;
    const http = /^HTTP (\d+)$/.exec(m);
    if (http) return t.common.errors.http(http[1]);
    // 只有请求没到面板（网络错误、没有说明）时才换成通用文字；面板自己返回的 Unavailable（例如「节点 n2 离线」）照原文显示。
    if (m === "" || /failed to fetch|networkerror|load failed/i.test(m)) return t.common.errors.unreachable;
    return m;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}

// isNotFound：接口明确说资源不存在，不用重试。
export function isNotFound(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.NotFound;
}

// isUnauthenticated：没登录或登录已失效。要求重新验证的不算（会话还在，验证后继续）。
export function isUnauthenticated(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Unauthenticated && !reverifyReason(err);
}

export function toDate(ts: Timestamp | undefined): Date | undefined {
  return ts ? timestampDate(ts) : undefined;
}
