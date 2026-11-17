// 会话密钥（见 docs/panel.md#会话）：登录后用 WebCrypto 生成一对 ECDSA P-256 密钥，私钥不可导出，存进 IndexedDB；
// 公钥交给面板记在会话上。面板每 15 分钟在响应头里给一个挑战，这里用私钥签名换发新令牌。
// 只偷到 cookie 的人拿不出签名。同一个浏览器的多个标签页共用一份私钥。
import { authClient, setSessionHandlers } from "@/lib/api";

const dbName = "nagi";
const storeName = "keys";
const keyName = "session";

function openDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(dbName, 1);
    req.onupgradeneeded = () => req.result.createObjectStore(storeName);
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

async function loadKey(): Promise<CryptoKeyPair | undefined> {
  try {
    const db = await openDB();
    return await new Promise((resolve, reject) => {
      const req = db.transaction(storeName, "readonly").objectStore(storeName).get(keyName);
      req.onsuccess = () => resolve(req.result as CryptoKeyPair | undefined);
      req.onerror = () => reject(req.error);
    });
  } catch {
    return undefined;
  }
}

// saveKey 只在还没有密钥时存入（add，不覆盖）：几个标签页同时生成时，后到的不会换掉别人已经登记的那份。
// 已经有了就返回 false，调用方改用存着的那份。
async function saveKey(k: CryptoKeyPair): Promise<boolean> {
  const db = await openDB();
  return new Promise<boolean>((resolve, reject) => {
    const tx = db.transaction(storeName, "readwrite");
    const req = tx.objectStore(storeName).add(k, keyName);
    req.onerror = (e) => {
      if (req.error?.name === "ConstraintError") {
        e.preventDefault();
        resolve(false);
      }
    };
    tx.oncomplete = () => resolve(true);
    tx.onerror = () => reject(tx.error);
  });
}

// withKeyLock：生成和读取密钥时各标签页排队（Web Locks），不支持的浏览器直接执行，靠 saveKey 不覆盖兜底。
function withKeyLock<T>(fn: () => Promise<T>): Promise<T> {
  if (navigator.locks?.request) return navigator.locks.request("nagi-session-key", fn) as Promise<T>;
  return fn();
}

// ensureSessionKey：登录、重新验证以后调用。没有密钥时生成一对（私钥不可导出），把公钥登记到当前会话。
// 面板只在登录后的一小段时间内接受登记，已经登记过的会话会拒绝，这里不当作错误。
export async function ensureSessionKey() {
  if (!window.crypto?.subtle || !window.indexedDB) return;
  try {
    const k = await withKeyLock(async () => {
      const stored = await loadKey();
      if (stored) return stored;
      const fresh = (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign", "verify"])) as CryptoKeyPair;
      return (await saveKey(fresh)) ? fresh : await loadKey();
    });
    if (!k) return;
    const spki = new Uint8Array(await crypto.subtle.exportKey("spki", k.publicKey));
    await authClient.registerSessionKey({ publicKey: spki });
  } catch {
    // 无痕窗口等存不下密钥时，到轮换时间面板会要求重新验证，不影响现在使用。
  }
}

let rotating: Promise<void> | undefined;

// rotate：用私钥签名挑战，换发新令牌。同时只做一次。
function rotate(challenge: string) {
  if (rotating) return;
  rotating = (async () => {
    const k = await loadKey();
    if (!k) return;
    const sig = await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, k.privateKey, new TextEncoder().encode(challenge));
    // 带上签的是哪个挑战：签名期间挑战换了时，面板不会当成签名不符。
    await authClient.rotateSession({ signature: new Uint8Array(sig), challenge });
  })()
    .catch(() => {
      // 签名失败时面板会要求重新验证，界面在那时处理。
    })
    .finally(() => {
      rotating = undefined;
    });
}

setSessionHandlers({ challenge: rotate });
