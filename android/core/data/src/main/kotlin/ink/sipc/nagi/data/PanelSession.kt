package ink.sipc.nagi.data

import com.connectrpc.Code
import com.google.protobuf.Timestamp
import ink.sipc.nagi.network.DeviceRemovedException
import ink.sipc.nagi.network.PanelClient
import ink.sipc.nagi.network.PanelException
import ink.sipc.nagi.network.TokenManager
import ink.sipc.nagi.network.UnlockRequiredException
import ink.sipc.nagi.network.orThrow
import ink.sipc.nagi.network.toPanelException
import ink.sipc.nagi.proto.v1.AppInfoRequest
import ink.sipc.nagi.proto.v1.AppInfoResponse
import ink.sipc.nagi.proto.v1.AppLogoutRequest
import ink.sipc.nagi.proto.v1.ApproveNodeRequest
import ink.sipc.nagi.proto.v1.Overview
import ink.sipc.nagi.proto.v1.QueryTrafficRequest
import ink.sipc.nagi.proto.v1.SetPanelNameRequest
import ink.sipc.nagi.proto.v1.GetSettingsRequest
import ink.sipc.nagi.proto.v1.PanelSettings
import ink.sipc.nagi.proto.v1.TrafficHistory
import ink.sipc.nagi.proto.v1.WatchOverviewRequest
import ink.sipc.nagi.security.AppKeys
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.withContext
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.channelFlow
import kotlinx.coroutines.selects.onTimeout
import kotlinx.coroutines.selects.select
import kotlinx.coroutines.withTimeoutOrNull

/** 首页的状态（见 docs/android.md#订阅流、s8 状态）。 */
sealed interface OverviewState {
    data object Loading : OverviewState

    /** live 为 false 时连不上面板，显示的是 updatedAt 时的数据，error 是原因。 */
    data class Ready(val overview: Overview, val live: Boolean, val updatedAt: Long, val error: String? = null) : OverviewState

    /** 还没有数据就出错了。unreachable 表示连不上面板（不是面板返回的错误）。 */
    data class Failed(val message: String, val unreachable: Boolean) : OverviewState

    /** 要先解锁 App。 */
    data object Locked : OverviewState

    /** 这台手机已经退出。 */
    data object Removed : OverviewState
}

/** 流量趋势的时间范围。 */
enum class TrafficRange(val seconds: Long, val step: Long) {
    HOUR(3600, 20),
    DAY(86400, 600),
    WEEK(7 * 86400, 3600),
}

/** 当前面板的连接：客户端、令牌和各页面的数据。 */
class PanelSession(
    val panel: Panel,
    private val keys: AppKeys,
    private val cache: SessionCache? = null,
    private val store: PanelStore? = null,
) {
    private val _current = MutableStateFlow(panel)

    /**
     * 面板记录的最新内容：面板名称会被管理员改掉（订阅流和 Info 带回新的名称时更新），本机的显示名称在设置里改。
     * 地址、指纹、设备 ID 这些不会变，仍然用 [panel]。
     */
    val current: StateFlow<Panel> = _current

    /** 改本机的显示名称，为空时显示面板名称。 */
    fun setAlias(alias: String) = update(_current.value.copy(alias = alias.trim()))

    /** 面板说它现在叫什么（订阅流、Info）：和记下的不一样时更新。 */
    fun panelNameIs(name: String) {
        if (name.isNotBlank() && name != _current.value.name) update(_current.value.copy(name = name))
    }

    private fun update(p: Panel) {
        _current.value = p
        store?.save(p)
    }

    /** 面板设置（管理员）：名称、默认名称等。 */
    suspend fun panelSettings(): PanelSettings =
        tokens.call { h -> client.settings.getSettings(GetSettingsRequest.getDefaultInstance(), h).orThrow() }.settings

    /** 修改面板名称（管理员），为空时恢复成主机名。返回面板现在的名称。 */
    suspend fun setPanelName(name: String): String {
        val res = tokens.call { h -> client.settings.setPanelName(SetPanelNameRequest.newBuilder().setName(name).build(), h).orThrow() }
        panelNameIs(res.settings.name)
        return res.settings.name
    }

    val client = PanelClient(panel.address, panel.fingerprint)
    val tokens = TokenManager(
        client, keys, panel.id, panel.deviceId, panel.address,
        loadChallenge = { cache?.challenge(panel.id) },
        saveChallenge = { c, at -> cache?.saveChallenge(panel.id, c, at) },
    )
    private val reconnect = Channel<Unit>(Channel.CONFLATED)

    /** 确认密钥的 Signature，交给系统验证框。 */
    fun confirmSignature() = keys.confirmSignature(panel.id)

    suspend fun info(): AppInfoResponse =
        tokens.call { h -> client.app.info(AppInfoRequest.getDefaultInstance(), h).orThrow() }.also { panelNameIs(it.panelName) }

    /** 立即重新连接订阅流（下拉刷新、点重试）。 */
    fun refresh() {
        reconnect.trySend(Unit)
    }

    /**
     * 首页的订阅流：打开时先收到完整状态，之后每次变化一份。断开后退避重连（1、2、5、10 秒，之后每 30 秒），
     * 重连后先到的完整状态覆盖旧数据。150 秒收不到任何消息（面板每 30 秒发心跳）当作断开。
     */
    fun overview(): Flow<OverviewState> = channelFlow {
        var last: Overview? = null
        var lastAt = 0L
        var attempt = 0
        var offline: Job? = null
        // 先显示上次的数据（本机加密缓存），同时连接面板；连上以后由订阅流覆盖。连不上时按缓存的时间显示「连不上面板」。
        val cached = withContext(Dispatchers.IO) { runCatching { cache?.overview(panel.id) }.getOrNull() }
        if (cached != null) {
            last = cached.first
            lastAt = cached.second
            send(OverviewState.Ready(cached.first, true, cached.second))
        } else {
            send(OverviewState.Loading)
        }
        while (true) {
            var error: PanelException? = null
            try {
                val stream = tokens.call { h -> client.projects.watchOverview(h) }
                stream.sendAndClose(WatchOverviewRequest.getDefaultInstance())
                val ch = stream.responseChannel()
                try {
                    while (true) {
                        val r = withTimeoutOrNull(STREAM_IDLE_MS) { ch.receiveCatching() } ?: break
                        if (r.isClosed) {
                            r.exceptionOrNull()?.let { throw it }
                            break
                        }
                        val m = r.getOrThrow()
                        if (m.heartbeat) continue
                        panelNameIs(m.panelName)
                        last = m
                        lastAt = System.currentTimeMillis()
                        attempt = 0
                        offline?.cancel()
                        offline = null
                        send(OverviewState.Ready(m, true, lastAt))
                        val at = lastAt
                        launch(Dispatchers.IO) { runCatching { cache?.saveOverview(panel.id, m, at) } }
                    }
                } finally {
                    stream.receiveClose()
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Throwable) {
                error = e.toPanelException()
            }
            when {
                error is DeviceRemovedException -> {
                    send(OverviewState.Removed)
                    return@channelFlow
                }
                error is UnlockRequiredException -> {
                    send(OverviewState.Locked)
                    return@channelFlow
                }
                // 令牌在流里过期或换了网段：重新换取后马上重连。
                error != null && error.code == Code.UNAUTHENTICATED && error.retoken != null -> {
                    tokens.invalidate()
                    continue
                }
            }
            val msg = error?.message ?: ""
            val unreachable = error == null || error.code == Code.UNAVAILABLE
            val l = last
            if (l == null) {
                send(OverviewState.Failed(msg, unreachable))
            } else if (offline == null) {
                // 有数据时先静默重连：网络切换、连接被回收这类短暂的断开很常见，重连一次就好。
                // 断开超过 OFFLINE_GRACE_MS 还没连上，才显示「连不上面板」。
                offline = launch {
                    delay(OFFLINE_GRACE_MS)
                    send(OverviewState.Ready(l, false, lastAt, msg))
                }
            }
            // 面板正常关闭了流（error 为 null）：马上重连，不算一次失败。
            val wait = if (error == null && attempt == 0) 0L else BACKOFF_MS.getOrElse(attempt) { BACKOFF_MS.last() }
            if (error != null) attempt++
            select {
                reconnect.onReceive { attempt = 0 }
                onTimeout(wait) {}
            }
        }
    }

    /** 流量趋势（QueryTraffic）。 */
    suspend fun traffic(range: TrafficRange): TrafficHistory = tokens.call { h ->
        val from = Timestamp.newBuilder().setSeconds(System.currentTimeMillis() / 1000 - range.seconds).build()
        client.nodes.queryTraffic(QueryTrafficRequest.newBuilder().setFrom(from).setStep(range.step).build(), h).orThrow()
    }

    /** 批准节点（首页「需要处理」行尾的「批准」）。 */
    suspend fun approveNode(nodeId: String) {
        tokens.call { h -> client.nodes.approveNode(ApproveNodeRequest.newBuilder().setNodeId(nodeId).build(), h).orThrow() }
    }

    /** 退出：删除面板上的设备记录。 */
    suspend fun logout() {
        tokens.call { h -> client.app.logout(AppLogoutRequest.getDefaultInstance(), h).orThrow() }
    }

    private companion object {
        const val STREAM_IDLE_MS = 150_000L
        const val OFFLINE_GRACE_MS = 5_000L
        val BACKOFF_MS = longArrayOf(1_000, 2_000, 5_000, 10_000, 30_000)
    }
}
