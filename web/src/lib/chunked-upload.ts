// 分块上传（断点续传，见 docs/agent.md#存储、internal/server/api/chunkupload.go）。
//
// 每个文件先 begin 拿到上传 ID 和节点已经收到的长度，再从这里起每次传 8 MiB，最后 commit 核对整个文件的 SHA-256。
// 节点按「目标路径 + 大小 + 文件标识」认出同一个上传：断线、刷新页面以后重新选择同一个文件，从上次确认的位置接着传。
// 文件标识是名字、大小、修改时间和首尾各 64 KiB 内容的哈希，不用读整个文件。
import { locale, t } from "@/i18n";
import { Sha256, sha256Hex } from "@/lib/sha256";

export const chunkSize = 8 << 20;
const probeSize = 64 << 10;
const maxRetries = 6;

export type UploadState = "queued" | "preparing" | "verifying" | "uploading" | "retrying" | "paused" | "error" | "done";

export type UploadItem = {
  key: string; // 列表里的标识
  name: string;
  path: string;
  size: number;
  sent: number; // 节点已经确认收到的长度
  verified: number; // 接着传之前重新读过的长度（算整个文件的哈希要从头读）
  state: UploadState;
  speed: number; // 字节/秒
  error?: string;
};

// UploadTarget：上传到哪里。url 拼出带 op 等参数的地址；fail 把失败的响应转成错误（例如身份验证过期）。
export type UploadTarget = {
  url: (path: string, params: Record<string, string>) => string;
  fail?: (res: Response) => Promise<Error | undefined>;
};

class HTTPError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
  ) {
    super(message);
  }
}

async function failure(target: UploadTarget, res: Response): Promise<Error> {
  const custom = await target.fail?.(res);
  if (custom) return custom;
  const body = await res.json().catch(() => null);
  return new HTTPError(body?.message || t.common.errors.http(String(res.status)), res.status, body?.code ?? "");
}

// fileKey 是文件的标识：名字、大小、修改时间，加上首尾各 64 KiB 的哈希。
async function fileKey(file: File): Promise<string> {
  const head = new Uint8Array(await file.slice(0, probeSize).arrayBuffer());
  const tail = new Uint8Array(await file.slice(Math.max(0, file.size - probeSize)).arrayBuffer());
  const h = new Sha256().update(new TextEncoder().encode(`${file.name}\n${file.size}\n${file.lastModified}\n`)).update(head).update(tail);
  return h.hex();
}

const sleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve, reject) => {
    const id = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(id);
      reject(new DOMException("aborted", "AbortError"));
    });
  });

const isAbort = (err: unknown) => err instanceof DOMException && err.name === "AbortError";

// 网络断开、面板或节点暂时不可用时重试；其余的错误（参数、权限、磁盘满）直接失败。
const retryable = (err: unknown) => !(err instanceof HTTPError) || err.status >= 500 && err.status !== 507 || err.status === 429;

type Task = {
  item: UploadItem;
  file: File;
  target: UploadTarget;
  id?: string;
  hash?: Sha256;
  hashed: number; // hash 已经算到的位置
  ctrl?: AbortController;
  samples: [number, number][];
  resolve?: () => void;
};

// 未完成的上传记在浏览器里，刷新页面后提示可以接着传。
const pendingKey = "nagi:pending-uploads";

export type PendingUpload = { scope: string; id: string; name: string; path: string; size: number; sent: number };

export function pendingUploads(scope: string): PendingUpload[] {
  try {
    return (JSON.parse(localStorage.getItem(pendingKey) ?? "[]") as PendingUpload[]).filter((p) => p.scope === scope);
  } catch {
    return [];
  }
}

function savePending(update: (list: PendingUpload[]) => PendingUpload[]) {
  try {
    const list = JSON.parse(localStorage.getItem(pendingKey) ?? "[]") as PendingUpload[];
    localStorage.setItem(pendingKey, JSON.stringify(update(list).slice(-50)));
  } catch {
    // 隐私模式等情况下记不住，不影响上传。
  }
}

export function forgetPending(scope: string, path: string) {
  savePending((list) => list.filter((p) => !(p.scope === scope && p.path === path)));
}

// UploadQueue：一个页面的上传队列，最多同时传 concurrency 个文件。界面用 subscribe 和 items 显示进度。
export class UploadQueue {
  private tasks: Task[] = [];
  private listeners = new Set<() => void>();
  private snapshot: UploadItem[] = [];
  private seq = 0;

  constructor(
    readonly scope: string,
    readonly concurrency: number,
  ) {}

  subscribe = (fn: () => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  items = () => this.snapshot;

  private emit() {
    this.snapshot = this.tasks.map((x) => ({ ...x.item }));
    this.listeners.forEach((fn) => fn());
  }

  // add 把文件加进队列，返回全部传完（或失败、取消）时完成的 Promise。
  add(files: { file: File; path: string }[], target: UploadTarget): Promise<void> {
    const added = files.map(({ file, path }) => {
      // 同一个目标已经在队列里（例如失败后重新选择）：换成新的。
      const old = this.tasks.find((x) => x.item.path === path && x.item.state !== "done");
      if (old) this.cancel(old.item.key, false);
      const task: Task = {
        item: { key: String(++this.seq), name: file.name, path, size: file.size, sent: 0, verified: 0, state: "queued", speed: 0 },
        file,
        target,
        hashed: 0,
        samples: [],
      };
      this.tasks.push(task);
      return new Promise<void>((resolve) => (task.resolve = resolve));
    });
    this.emit();
    this.pump();
    return Promise.all(added).then(() => undefined);
  }

  private running() {
    return this.tasks.filter((x) => ["preparing", "verifying", "uploading", "retrying"].includes(x.item.state)).length;
  }

  private pump() {
    for (const task of this.tasks) {
      if (this.running() >= this.concurrency) return;
      if (task.item.state === "queued") void this.run(task);
    }
  }

  pause(key: string) {
    const task = this.tasks.find((x) => x.item.key === key);
    if (!task || task.item.state === "done" || task.item.state === "error") return;
    task.ctrl?.abort();
    task.item.state = "paused";
    task.item.speed = 0;
    this.emit();
    this.pump();
  }

  resume(key: string) {
    const task = this.tasks.find((x) => x.item.key === key);
    if (!task || (task.item.state !== "paused" && task.item.state !== "error")) return;
    task.item.state = "queued";
    task.item.error = undefined;
    this.emit();
    this.pump();
  }

  // cancel 取消上传，节点删掉已经收到的部分。
  cancel(key: string, notify = true) {
    const task = this.tasks.find((x) => x.item.key === key);
    if (!task) return;
    task.ctrl?.abort();
    this.tasks = this.tasks.filter((x) => x !== task);
    if (task.id && task.item.state !== "done") {
      void fetch(task.target.url(task.item.path, { op: "abort", id: task.id }), { method: "DELETE", headers: { "X-Nagi-Upload": "1", "Accept-Language": locale } }).catch(() => {});
    }
    forgetPending(this.scope, task.item.path);
    task.resolve?.();
    if (notify) this.emit();
    this.pump();
  }

  // clearDone 把传完的从列表里去掉。
  clearDone() {
    this.tasks = this.tasks.filter((x) => x.item.state !== "done");
    this.emit();
  }

  private progress(task: Task) {
    const now = performance.now();
    task.samples.push([now, task.item.sent]);
    while (task.samples.length > 2 && now - task.samples[0][0] > 10_000) task.samples.shift();
    const [t0, s0] = task.samples[0];
    task.item.speed = now - t0 > 500 ? ((task.item.sent - s0) * 1000) / (now - t0) : 0;
    savePending((list) => [
      ...list.filter((p) => !(p.scope === this.scope && p.path === task.item.path)),
      { scope: this.scope, id: task.id ?? "", name: task.item.name, path: task.item.path, size: task.item.size, sent: task.item.sent },
    ]);
    this.emit();
  }

  private async request(task: Task, params: Record<string, string>, init: RequestInit): Promise<Record<string, unknown>> {
    const res = await fetch(task.target.url(task.item.path, params), {
      ...init,
      signal: task.ctrl?.signal,
      headers: { "X-Nagi-Upload": "1", "Accept-Language": locale, ...(init.headers ?? {}) },
    });
    if (!res.ok) throw await failure(task.target, res);
    return res.json();
  }

  // begin 开始或接着上传，返回节点已经收到的长度。
  private async begin(task: Task): Promise<number> {
    const key = await fileKey(task.file);
    const res = await this.request(task, { op: "begin", size: String(task.file.size), key }, { method: "POST" });
    task.id = String(res.id);
    return Number(res.offset);
  }

  // rehash 从头读到 offset，算出这一段的哈希（接着传时节点上的部分浏览器这边要重新读一遍）。
  private async rehash(task: Task, offset: number, signal: AbortSignal) {
    if (task.hash && task.hashed === offset) return;
    task.hash = new Sha256();
    task.hashed = 0;
    task.item.state = "verifying";
    while (task.hashed < offset) {
      if (signal.aborted) throw new DOMException("aborted", "AbortError");
      const end = Math.min(offset, task.hashed + chunkSize);
      task.hash.update(new Uint8Array(await task.file.slice(task.hashed, end).arrayBuffer()));
      task.hashed = end;
      task.item.verified = end;
      this.emit();
    }
  }

  private async run(task: Task) {
    const ctrl = new AbortController();
    task.ctrl = ctrl;
    task.item.state = "preparing";
    task.samples = [];
    this.emit();
    try {
      let offset = await this.withRetry(task, () => this.begin(task));
      task.item.sent = offset;
      await this.rehash(task, offset, ctrl.signal);
      task.item.state = "uploading";
      this.progress(task);
      while (offset < task.file.size) {
        const end = Math.min(task.file.size, offset + chunkSize);
        const chunk = new Uint8Array(await task.file.slice(offset, end).arrayBuffer());
        const sum = await sha256Hex(chunk);
        let next: number;
        try {
          next = await this.withRetry(task, async () => {
            const res = await this.request(task, { op: "part", id: task.id!, offset: String(offset) }, { method: "PUT", body: chunk as Uint8Array<ArrayBuffer>, headers: { "X-Nagi-Chunk-Sha256": sum } });
            return Number(res.offset);
          });
        } catch (err) {
          if (!(err instanceof HTTPError) || (err.status !== 409 && err.status !== 404)) throw err;
          // 409：节点收到的长度和这里不一样（例如上一块其实已经收到了）；404：上传失效。重新开始一次，按节点的为准。
          if (err.status === 404) task.hash = undefined;
          const server = await this.withRetry(task, () => this.begin(task));
          task.item.sent = server;
          if (task.hash && server === offset + chunk.length) {
            task.hash.update(chunk);
            task.hashed = server;
          } else await this.rehash(task, server, ctrl.signal);
          offset = server;
          task.item.state = "uploading";
          task.item.sent = offset;
          this.progress(task);
          continue;
        }
        task.hash!.update(chunk);
        task.hashed = next;
        offset = next;
        task.item.sent = offset;
        task.item.state = "uploading";
        this.progress(task);
      }
      task.item.state = "verifying";
      task.item.verified = task.file.size;
      this.emit();
      await this.withRetry(task, () => this.request(task, { op: "commit", id: task.id!, sha256: task.hash!.hex() }, { method: "POST" }));
      task.item.state = "done";
      task.item.speed = 0;
      forgetPending(this.scope, task.item.path);
      this.emit();
      task.resolve?.();
    } catch (err) {
      if (isAbort(err) || ctrl.signal.aborted) return; // 暂停或取消
      task.item.state = "error";
      task.item.speed = 0;
      task.item.error = err instanceof Error ? err.message : String(err);
      task.hash = undefined; // 校验不一致时整个上传作废，下次从节点给的位置重新算
      this.emit();
      task.resolve?.();
    } finally {
      if (task.ctrl === ctrl) task.ctrl = undefined;
      this.pump();
    }
  }

  // withRetry 在网络断开等暂时的错误时等一会儿重试，等待时显示「正在重试」。
  private async withRetry<T>(task: Task, fn: () => Promise<T>): Promise<T> {
    for (let attempt = 0; ; attempt++) {
      try {
        const v = await fn();
        if (task.item.state === "retrying") {
          task.item.state = "uploading";
          this.emit();
        }
        return v;
      } catch (err) {
        if (isAbort(err) || !retryable(err) || attempt >= maxRetries || !task.ctrl) throw err;
        task.item.state = "retrying";
        task.item.speed = 0;
        this.emit();
        await sleep(Math.min(16_000, 1000 * 2 ** attempt), task.ctrl.signal);
      }
    }
  }

  // active 表示还有没传完的（离开页面前提醒）。
  active() {
    return this.tasks.some((x) => x.item.state !== "done" && x.item.state !== "error" && x.item.state !== "paused");
  }
}
