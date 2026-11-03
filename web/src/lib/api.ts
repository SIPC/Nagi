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
const transport = createConnectTransport({
  baseUrl: window.location.origin,
  interceptors: [
    (next) => (req) => {
      req.header.set("Accept-Language", locale);
      return next(req);
    },
  ],
});

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
    if (err.code === Code.Unavailable || /failed to fetch|networkerror|load failed/i.test(m)) return t.common.errors.unreachable;
    return m;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}

// isNotFound：接口明确说资源不存在，不用重试。
export function isNotFound(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.NotFound;
}

export function isUnauthenticated(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Unauthenticated;
}

export function toDate(ts: Timestamp | undefined): Date | undefined {
  return ts ? timestampDate(ts) : undefined;
}
