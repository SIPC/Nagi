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

async function saveKey(k: CryptoKeyPair) {
  const db = await openDB();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(storeName, "readwrite");
    tx.objectStore(storeName).put(k, keyName);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

// ensureSessionKey：登录、重新验证以后调用。没有密钥时生成一对（私钥不可导出），把公钥登记到当前会话。
// 面板只在登录后的一小段时间内接受登记，已经登记过的会话会拒绝，这里不当作错误。
export async function ensureSessionKey() {
  if (!window.crypto?.subtle || !window.indexedDB) return;
  try {
    let k = await loadKey();
    if (!k) {
      k = (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign", "verify"])) as CryptoKeyPair;
      await saveKey(k);
    }
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
    await authClient.rotateSession({ signature: new Uint8Array(sig) });
  })()
    .catch(() => {
      // 签名失败时面板会要求重新验证，界面在那时处理。
    })
    .finally(() => {
      rotating = undefined;
    });
}

setSessionHandlers({ challenge: rotate });
