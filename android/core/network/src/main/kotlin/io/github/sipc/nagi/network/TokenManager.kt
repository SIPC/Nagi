package io.github.sipc.nagi.network

import com.connectrpc.Code
import io.github.sipc.nagi.proto.v1.AppChallengeRequest
import io.github.sipc.nagi.proto.v1.AppTokenRequest
import io.github.sipc.nagi.security.AppKeys
import io.github.sipc.nagi.security.KeyKind
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

// 令牌（见 docs/android.md#换取令牌）：用设备密钥签名换取，只在内存里，10 分钟过期。App 在前台时每 4 分钟换新的，
// 同时把面板上的解锁时间往后延。面板说换了网段、过期了、面板重启过时自动重新换取，用户看不到。

class TokenManager(
    private val client: PanelClient,
    private val keys: AppKeys,
    private val panelId: String,
    private val deviceId: String,
    /** 签名里写的主机：连接的 对外地址:端口。 */
    private val host: String,
    private val now: () -> Long = System::currentTimeMillis,
) {
    private val mutex = Mutex()
    private var token: String? = null
    private var obtainedAt = 0L
    private var expiresAt = 0L
    private var challenge: String? = null
    private var challengeAt = 0L

    /** 换取以后多久换新的。 */
    var refreshAfterMs = 4 * 60_000L

    /** 解锁时长（分钟），面板在 Token 的回复里给。 */
    @Volatile
    var unlockMinutes = 15
        private set

    /** 当前可用的令牌，没有或快过期时用设备密钥换取。需要解锁时抛 UnlockRequiredException。 */
    suspend fun token(): String = mutex.withLock {
        val t = token
        if (t != null && now() - obtainedAt < refreshAfterMs && now() < expiresAt - 30_000) return t
        exchangeLocked(KeyKind.DEVICE) { keys.sign(panelId, KeyKind.DEVICE, it) }
    }

    /** 用确认密钥换取（解锁 App）。[sign] 弹出系统验证框后签名。 */
    suspend fun unlock(sign: suspend (ByteArray) -> ByteArray): String = mutex.withLock {
        exchangeLocked(KeyKind.CONFIRM, sign)
    }

    /** 丢掉令牌，下一次请求重新换取（网络切换、面板要求时）。 */
    fun invalidate() {
        token = null
    }

    /** 面板给的下一次用的挑战 15 分钟有效，留些余量。 */
    private suspend fun challengeLocked(): String {
        val c = challenge
        if (c != null && now() - challengeAt < 13 * 60_000) return c
        return client.app.challenge(AppChallengeRequest.newBuilder().setDeviceId(deviceId).build()).orThrow().challenge
    }

    private suspend fun exchangeLocked(kind: KeyKind, sign: suspend (ByteArray) -> ByteArray): String {
        var retried = false
        while (true) {
            val ch = challengeLocked()
            challenge = null
            val msg = "nagi-app-token\n$host\n$deviceId\n$ch".toByteArray()
            val req = AppTokenRequest.newBuilder().setDeviceId(deviceId).setChallenge(ch).setKey(kind.id).setHost(host)
                .setSignature(com.google.protobuf.ByteString.copyFrom(sign(msg))).build()
            try {
                val res = client.app.token(req).orThrow()
                token = res.token
                obtainedAt = now()
                expiresAt = now() + (res.expiresAt.seconds * 1000 - System.currentTimeMillis()).coerceIn(60_000, 10 * 60_000)
                challenge = res.nextChallenge
                challengeAt = now()
                if (res.unlockMinutes > 0) unlockMinutes = res.unlockMinutes
                return res.token
            } catch (e: PanelException) {
                when {
                    e.code == Code.NOT_FOUND -> throw DeviceRemovedException(e.message.orEmpty())
                    e.retoken == "unlock" -> throw UnlockRequiredException(e.message.orEmpty())
                    // 挑战过期（面板重启过）：要一个新的再试一次。
                    e.code == Code.FAILED_PRECONDITION && !retried -> retried = true
                    else -> throw e
                }
            }
        }
    }

    /** 带上令牌调用接口。面板说要重新换取时换一次再试。 */
    suspend fun <T> call(block: suspend (headers: Map<String, List<String>>) -> T): T {
        val first = runCatching { block(auth(token())) }
        val e = first.exceptionOrNull()?.toPanelException() ?: return first.getOrThrow()
        if (e.code != Code.UNAUTHENTICATED || e.retoken == null) throw e
        invalidate()
        return try {
            block(auth(token()))
        } catch (e: Throwable) {
            throw e.toPanelException()
        }
    }

    companion object {
        fun auth(token: String): Map<String, List<String>> = mapOf("Authorization" to listOf("Bearer $token"))
    }
}
