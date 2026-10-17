import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import {
  AuthService,
  NodeService,
  ProjectService,
  SettingsService,
  SetupService,
} from "@/gen/nagi/v1/api_pb";

// 浏览器与面板同源，cookie 会随请求自动带上。
const transport = createConnectTransport({ baseUrl: window.location.origin });

export const setupClient = createClient(SetupService, transport);
export const authClient = createClient(AuthService, transport);
export const nodeClient = createClient(NodeService, transport);
export const projectClient = createClient(ProjectService, transport);
export const settingsClient = createClient(SettingsService, transport);

export function errorMessage(err: unknown): string {
  if (err instanceof ConnectError) return err.rawMessage;
  if (err instanceof Error) return err.message;
  return String(err);
}

export function isUnauthenticated(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Unauthenticated;
}

export function toDate(ts: Timestamp | undefined): Date | undefined {
  return ts ? timestampDate(ts) : undefined;
}
