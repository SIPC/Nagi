import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { create } from "@bufbuild/protobuf";
import {
  AuthService,
  type ListNodesResponse,
  ListNodesResponseSchema,
  NodeService,
  ProjectService,
  SettingsService,
  SetupService,
} from "@/gen/nagi/v1/api_pb";
import { locale, t } from "@/i18n";
import { NodeLogService } from "@/gen/nagi/v1/logs_pb";
import { ModelReportService } from "@/gen/nagi/v1/model_report_pb";
import { AlertService } from "@/gen/nagi/v1/alert_state_pb";

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
// 没有「查看节点」权限时节点列表当作空的（面板拒绝），页面不用到处处理：节点一律显示为「其他节点」。
const rawNodeClient = createClient(NodeService, transport);
export const nodeClient: typeof rawNodeClient = {
  ...rawNodeClient,
  listNodes: async (...args: Parameters<typeof rawNodeClient.listNodes>) => {
    try {
      return await rawNodeClient.listNodes(...args);
    } catch (err) {
      if (err instanceof ConnectError && err.code === Code.PermissionDenied) return create(ListNodesResponseSchema, {});
      throw err;
    }
  },
};
export const projectClient = createClient(ProjectService, transport);
export const settingsClient = createClient(SettingsService, transport);
export const logClient = createClient(NodeLogService, transport);
export const modelReportClient = createClient(ModelReportService, transport);
export const alertClient = createClient(AlertService, transport);

// knownNodes：查节点名称和在线状态用的节点列表；列出、选择节点只用 nodes。看不到的节点（访问范围外，或者没有「查看节点」权限）
// 面板不给 ID 和名称，回复里是空的 ID，显示为「其他节点」，也不显示是否在线（见 docs/panel.md#访问范围）。
export const knownNodes = (res: ListNodesResponse | undefined) => (res ? [...res.nodes, ...res.otherNodes] : []);

// orOther：节点名称查不到时显示的文字。空的 ID 是看不到的节点。
export const orOther = (id: string) => id || t.common.otherNode;

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
