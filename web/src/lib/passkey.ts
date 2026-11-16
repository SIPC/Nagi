// 通行密钥（见 docs/panel.md#通行密钥）：调用浏览器的 WebAuthn，把结果原样交给面板校验。
import { authClient, settingsClient } from "@/lib/api";

// passkeySupported：浏览器支持 WebAuthn，并且当前打开的就是绑定的域名（通行密钥只对这个域名有效）。
export function passkeySupported(domain: string | undefined): boolean {
  return (
    !!domain &&
    typeof window.PublicKeyCredential === "function" &&
    window.location.hostname.toLowerCase() === domain.toLowerCase()
  );
}

// COSE 算法：ES256、EdDSA、RS256（和面板 auth/webauthn.go 的 coseAlgorithms 一致）。
const algorithms: PublicKeyCredentialParameters[] = [-7, -8, -257].map(
  (alg) => ({ type: "public-key", alg }),
);

const buf = (b: Uint8Array) => b.slice().buffer as ArrayBuffer;
const bytes = (b: ArrayBuffer) => new Uint8Array(b);

// cancelled：用户在系统对话框里取消，或者超时。界面上不当成错误。
export function cancelled(err: unknown): boolean {
  return (
    err instanceof DOMException &&
    (err.name === "NotAllowedError" || err.name === "AbortError")
  );
}

// addPasskey 在浏览器里创建通行密钥并保存到面板。调用前要先做高权限确认（见 BeginPasskeyRegistration）。
export async function addPasskey(name: string) {
  const r = await settingsClient.beginPasskeyRegistration({});
  const cred = (await navigator.credentials.create({
    publicKey: {
      challenge: buf(r.challenge),
      rp: { id: r.rpId, name: "Nagi" },
      user: { id: buf(r.userId), name: r.userName, displayName: r.displayName },
      pubKeyCredParams: algorithms,
      excludeCredentials: r.excludeCredentials.map((id) => ({
        type: "public-key" as const,
        id: buf(id),
      })),
      authenticatorSelection: {
        residentKey: "required",
        requireResidentKey: true,
        userVerification: "required",
      },
      attestation: "none",
      timeout: 120_000,
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new DOMException("cancelled", "NotAllowedError");
  const res = cred.response as AuthenticatorAttestationResponse;
  return settingsClient.addPasskey({
    challengeId: r.challengeId,
    name,
    clientDataJson: bytes(res.clientDataJSON),
    attestationObject: bytes(res.attestationObject),
  });
}

// loginWithPasskey 用通行密钥登录：不填用户名，浏览器列出这个域名下的通行密钥。
export async function loginWithPasskey() {
  const c = await authClient.beginPasskeyLogin({});
  const cred = (await navigator.credentials.get({
    publicKey: {
      challenge: buf(c.challenge),
      rpId: c.rpId,
      userVerification: "required",
      timeout: 120_000,
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new DOMException("cancelled", "NotAllowedError");
  const res = cred.response as AuthenticatorAssertionResponse;
  return authClient.passkeyLogin({
    challengeId: c.challengeId,
    credentialId: bytes(cred.rawId),
    clientDataJson: bytes(res.clientDataJSON),
    authenticatorData: bytes(res.authenticatorData),
    signature: bytes(res.signature),
    userHandle: res.userHandle ? bytes(res.userHandle) : new Uint8Array(),
  });
}

// defaultPasskeyName 是新通行密钥的默认名称：浏览器和系统，例如「Chrome · Windows」。
export function defaultPasskeyName(): string {
  const ua = navigator.userAgent;
  const browser = /Edg\//.test(ua)
    ? "Edge"
    : /Firefox\//.test(ua)
      ? "Firefox"
      : /Chrome\//.test(ua)
        ? "Chrome"
        : /Safari\//.test(ua)
          ? "Safari"
          : "";
  const os = /iPhone|iPad/.test(ua)
    ? "iOS"
    : /Android/.test(ua)
      ? "Android"
      : /Mac OS X/.test(ua)
        ? "macOS"
        : /Windows/.test(ua)
          ? "Windows"
          : /Linux/.test(ua)
            ? "Linux"
            : "";
  return [browser, os].filter(Boolean).join(" · ");
}
