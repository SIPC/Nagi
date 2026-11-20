package ink.sipc.nagi.data

import com.connectrpc.Code
import ink.sipc.nagi.network.ElevationRequiredException
import ink.sipc.nagi.network.PanelException
import ink.sipc.nagi.network.UnreachableException
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.suspendCancellableCoroutine
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString
import okio.ByteString.Companion.toByteString
import org.json.JSONObject
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

// 节点终端的连接（docs/node-access.md#终端、docs/android.md#日志终端和编辑器）：和网页一样连面板的 /ws/node-shell，
// 面板经控制连接转到节点上 root 的 shell。消息格式见 internal/server/api/terminal.go：二进制是输入输出，
// 文本是 JSON（resize；面板发来的 exit、error）。带 App 令牌（Authorization），面板按设备重查（被退出、降权就断开）。

/** 终端连接上发生的事。 */
sealed interface ShellEvent {
    /** shell 退出了（exit 的退出码）。 */
    data class Exited(val code: Int) : ShellEvent

    /** 面板说出错了（例如空闲太久、权限变了）。 */
    data class Failed(val message: String) : ShellEvent

    /** 连接断了（网络、面板重启）。 */
    data class Closed(val message: String?) : ShellEvent
}

/** 一条打开着的终端连接。 */
class ShellConnection internal constructor(private val ws: WebSocket) {
    fun send(bytes: ByteArray) {
        ws.send(bytes.toByteString())
    }

    fun resize(cols: Int, rows: Int) {
        ws.send(JSONObject().put("type", "resize").put("cols", cols).put("rows", rows).toString())
    }

    fun close() {
        ws.close(1000, null)
    }
}

/**
 * 打开节点 [nodeId] 的终端，[cwd] 是起始目录（文件页的「在此打开终端」，空为 root 的家目录）。
 * 连上以后才返回；面板要求先做高权限确认时抛 [ElevationRequiredException]，令牌要重新换取时由 TokenManager 换了再试。
 * [onOutput]、[onEvent] 在 OkHttp 的线程上调用。
 */
suspend fun PanelSession.openNodeShell(
    nodeId: String,
    cwd: String,
    cols: Int,
    rows: Int,
    onOutput: (ByteArray) -> Unit,
    onEvent: (ShellEvent) -> Unit,
): ShellConnection = tokens.call { headers ->
    val url = "https://${panel.address}/ws/node-shell".toHttpUrl().newBuilder()
        .addQueryParameter("node", nodeId).addQueryParameter("cwd", cwd)
        .addQueryParameter("cols", cols.toString()).addQueryParameter("rows", rows.toString())
        .build()
    val req = Request.Builder().url(url).apply { headers.forEach { (k, v) -> v.forEach { addHeader(k, it) } } }.build()
    // 终端可能很久没有输出：定时 ping，免得中间的设备把连接当成空闲断掉。
    val http = client.http.newBuilder().pingInterval(30, TimeUnit.SECONDS).build()
    suspendCancellableCoroutine { cont ->
        val opened = AtomicBoolean(false)
        val ws = http.newWebSocket(req, object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                opened.set(true)
                cont.resume(ShellConnection(webSocket))
            }

            override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
                onOutput(bytes.toByteArray())
            }

            override fun onMessage(webSocket: WebSocket, text: String) {
                val msg = runCatching { JSONObject(text) }.getOrNull() ?: return
                when (msg.optString("type")) {
                    "exit" -> onEvent(ShellEvent.Exited(msg.optInt("code")))
                    "error" -> onEvent(ShellEvent.Failed(msg.optString("message")))
                }
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                onEvent(ShellEvent.Closed(null))
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                if (opened.get()) {
                    onEvent(ShellEvent.Closed(t.message))
                } else if (cont.isActive) {
                    cont.resumeWithException(upgradeError(t, response))
                }
            }
        })
        cont.invokeOnCancellation { ws.cancel() }
    }
}

/** 升级成 WebSocket 之前面板回的错误：和 connect 的错误一样换成 PanelException，令牌和高权限确认的处理才一致。 */
private fun upgradeError(t: Throwable, r: Response?): Exception {
    if (t is CancellationException) return t
    if (r == null) return UnreachableException(t.message ?: t.javaClass.simpleName, t)
    val body = runCatching { r.body.string() }.getOrNull()?.trim().orEmpty().ifEmpty { r.message }
    if (r.code == 403 && r.header("X-Nagi-Elevation") == "required") return ElevationRequiredException(body, t)
    val code = when (r.code) {
        401 -> Code.UNAUTHENTICATED
        403 -> Code.PERMISSION_DENIED
        404 -> Code.NOT_FOUND
        429 -> Code.RESOURCE_EXHAUSTED
        in 500..599 -> Code.UNAVAILABLE
        else -> Code.UNKNOWN
    }
    return PanelException(code, body, r.header("Nagi-App-Retoken"), t)
}
