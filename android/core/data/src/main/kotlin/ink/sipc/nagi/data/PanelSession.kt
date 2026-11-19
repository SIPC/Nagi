package ink.sipc.nagi.data

import ink.sipc.nagi.proto.v1.AppChallengeRequest
import ink.sipc.nagi.proto.v1.AppElevateRequest
import ink.sipc.nagi.proto.v1.AppApproveRequest
import ink.sipc.nagi.proto.v1.AppListApprovalsRequest
import ink.sipc.nagi.proto.v1.AppApproval
import com.google.protobuf.ByteString
import com.connectrpc.Code
import com.connectrpc.ResponseMessage
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
import ink.sipc.nagi.proto.v1.MeRequest
import ink.sipc.nagi.proto.v1.ApproveNodeRequest
import ink.sipc.nagi.proto.v1.SetPanelNameRequest
import ink.sipc.nagi.proto.v1.GetSettingsRequest
import ink.sipc.nagi.proto.v1.PanelSettings
import ink.sipc.nagi.security.AppKeys
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
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

/** 首页的状态（见 docs/android.md#订阅流、s8 状态）。 */
sealed interface OverviewState {
    data object Loading : OverviewState

    /** live 为 false 时连不上面板，显示的是 updatedAt 时的数据，error 是原因。 */
    data class Ready(val overview: HomeOverview, val live: Boolean, val updatedAt: Long, val error: String? = null) : OverviewState

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

    /**
     * 带上令牌调用一个接口：`session.rpc { c, h -> c.settings.getSecurity(req, h) }`。面板返回的错误抛 PanelException，
     * message 已按界面语言，可以直接显示。设置页的各个子页都用它。
     */
    suspend fun <T> rpc(block: suspend (PanelClient, Map<String, List<String>>) -> ResponseMessage<T>): T =
        tokens.call { h -> block(client, h).orThrow() }

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

    /** 马上再查一次首页（下拉刷新、点重试）。 */
    fun refresh() {
        reconnect.trySend(Unit)
    }

    private val home = HomeLoader(this)

    /** 自己的权限（决定入口和「需要处理」行尾有没有按钮），1 分钟查一次（角色的权限可能被改）。 */
    private var access: Access? = null
    private var accessAt = 0L

    suspend fun access(): Access {
        access?.let { if (System.currentTimeMillis() - accessAt < ACCESS_TTL_MS) return it }
        return Access.of(info()).also { access = it; accessAt = System.currentTimeMillis() }
    }

    /**
     * 首页的数据：和网页的全局概览一样用网页的接口查、在手机上算（见 HomeData.kt），每 [POLL_MS] 查一次，
     * 收集的人走了就停（HomeViewModel 在页面离开 5 秒后停止收集）。先显示上次的数据（本机加密缓存）。
     * 查不到时退避重试（1、2、5、10 秒，之后每 30 秒）；有数据时先静默重试，连续失败超过 [OFFLINE_GRACE_MS] 才显示「连不上面板」。
     */
    fun overview(): Flow<OverviewState> = channelFlow {
        var last: HomeOverview? = null
        var lastAt = 0L
        var attempt = 0
        var failingSince = 0L
        val cached = withContext(Dispatchers.IO) { runCatching { cache?.overview(panel.id) }.getOrNull() }
        if (cached != null) {
            last = cached.first
            lastAt = cached.second
            send(OverviewState.Ready(cached.first, true, cached.second))
        } else {
            send(OverviewState.Loading)
        }
        // 面板名称可能被管理员改过：access() 查 Info 时会更新记下的名称。
        while (true) {
            var error: PanelException? = null
            try {
                val o = home.load(access())
                last = o
                lastAt = System.currentTimeMillis()
                attempt = 0
                failingSince = 0L
                send(OverviewState.Ready(o, true, lastAt))
                val at = lastAt
                launch(Dispatchers.IO) { runCatching { cache?.saveOverview(panel.id, o, at) } }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Throwable) {
                error = e.toPanelException()
            }
            when (error) {
                is DeviceRemovedException -> {
                    send(OverviewState.Removed)
                    return@channelFlow
                }
                is UnlockRequiredException -> {
                    send(OverviewState.Locked)
                    return@channelFlow
                }
                null -> {}
                else -> {
                    val l = last
                    val unreachable = error.code == Code.UNAVAILABLE
                    if (failingSince == 0L) failingSince = System.currentTimeMillis()
                    if (l == null) send(OverviewState.Failed(error.message.orEmpty(), unreachable))
                    else if (System.currentTimeMillis() - failingSince >= OFFLINE_GRACE_MS) send(OverviewState.Ready(l, false, lastAt, error.message))
                }
            }
            val wait = if (error == null) POLL_MS else BACKOFF_MS.getOrElse(attempt) { BACKOFF_MS.last() }
            if (error != null) attempt++
            select {
                reconnect.onReceive { attempt = 0 }
                onTimeout(wait) {}
            }
        }
    }

    /** 流量趋势：和网页概览一样按节点查监控、在手机上合计。 */
    suspend fun traffic(range: TrafficRange): TrafficSeries = home.traffic(range.seconds, range.step)

    // ---- 手机确认和高权限确认（见 docs/android.md#手机确认）----

    /** 等这个成员在手机上确认的请求（网页的高权限确认、登录的第二步）。 */
    suspend fun approvals(): List<AppApproval> =
        rpc { c, h -> c.app.listApprovals(AppListApprovalsRequest.getDefaultInstance(), h) }.approvalsList

    /**
     * 确认一个请求：[sign] 弹出系统验证框后用确认密钥签名。签的是用途、连接的主机、设备 ID、请求 ID、种类和网页上的数字，
     * 所以这个签名只能确认这一个请求。
     */
    suspend fun approve(a: AppApproval, code: String, sign: suspend (ByteArray) -> ByteArray) {
        val host = panel.address
        val msg = listOf("nagi-app-approve", host, panel.deviceId, a.id, a.kind, code).joinToString("\n").toByteArray()
        val sig = sign(msg)
        rpc { c, h ->
            c.app.approve(AppApproveRequest.newBuilder().setId(a.id).setCode(code).setHost(host).setSignature(ByteString.copyFrom(sig)).build(), h)
        }
    }

    /** 拒绝一个请求（不用验证）。 */
    suspend fun denyApproval(id: String) {
        rpc { c, h -> c.app.approve(AppApproveRequest.newBuilder().setId(id).setDeny(true).build(), h) }
    }

    /** App 自己的高权限确认：用确认密钥签面板给的挑战，15 分钟内有效。 */
    suspend fun elevate(sign: suspend (ByteArray) -> ByteArray) {
        val host = panel.address
        val ch = client.app.challenge(AppChallengeRequest.newBuilder().setDeviceId(panel.deviceId).build()).orThrow().challenge
        val sig = sign(listOf("nagi-app-elevate", host, panel.deviceId, ch).joinToString("\n").toByteArray())
        rpc { c, h -> c.app.elevate(AppElevateRequest.newBuilder().setChallenge(ch).setHost(host).setSignature(ByteString.copyFrom(sig)).build(), h) }
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
        /** 首页多久查一次（和网页的概览一样）。 */
        const val POLL_MS = 10_000L
        const val ACCESS_TTL_MS = 60_000L
        const val OFFLINE_GRACE_MS = 5_000L
        val BACKOFF_MS = longArrayOf(1_000, 2_000, 5_000, 10_000, 30_000)
    }
}
