package ink.sipc.nagi.data

import com.connectrpc.Code
import com.google.protobuf.ByteString
import com.google.protobuf.Timestamp
import ink.sipc.nagi.network.DeviceRemovedException
import ink.sipc.nagi.network.PanelException
import ink.sipc.nagi.network.UnlockRequiredException
import ink.sipc.nagi.network.toPanelException
import ink.sipc.nagi.proto.v1.Container
import ink.sipc.nagi.proto.v1.FailoverParticipant
import ink.sipc.nagi.proto.v1.GetNodeMeshRequest
import ink.sipc.nagi.proto.v1.GetPanelMeshRequest
import ink.sipc.nagi.proto.v1.ListProjectsRequest
import ink.sipc.nagi.proto.v1.MeshPeerStatus
import ink.sipc.nagi.proto.v1.MeshStatus
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.proto.v1.ProjectSummary
import ink.sipc.nagi.proto.v1.QueryNodeMetricsRequest
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import java.util.Locale
import kotlin.math.PI
import kotlin.math.abs
import kotlin.math.cos
import kotlin.math.hypot
import kotlin.math.max
import kotlin.math.min
import kotlin.math.roundToLong
import kotlin.math.sin

// 节点网络和高可用（docs/android.md「节点网络」「高可用」「宽屏」）：拓扑图的布局、连接的合成和分类、故障模拟，
// 算法同网页的 pages/node-map.tsx。都是纯函数，坐标的单位是 dp，原点在圆心。

/** 图上面板的 ID（和节点 ID 不会重）。 */
const val MAP_PANEL = "__panel"

data class MapPoint(val x: Float, val y: Float)

/** 画布的范围：左上角和宽高。 */
data class MapBox(val x: Float, val y: Float, val w: Float, val h: Float)

/** [spacing] 是相邻两个节点圆心的距离，上下方的文字最宽不超过它。 */
data class MapLayout(val pos: Map<String, MapPoint>, val spacing: Float, val box: MapBox)

private fun dist(a: MapPoint, b: MapPoint) = hypot(a.x - b.x, a.y - b.y)

private fun mid(a: MapPoint, b: MapPoint) = MapPoint((a.x + b.x) / 2, (a.y + b.y) / 2)

/**
 * 面板和节点在圆上的位置。面板在最上面，不在圆心，节点之间的线不用绕开它。按可用宽度 [avail] 算，不整体缩放：
 * 放不下时先收窄两侧留给文字的宽度，再缩小圆，圆最小到相邻节点不相碰；还放不下时由页面横向滚动。[avail] 为 0 时按默认大小。
 */
fun mapLayout(ids: List<String>, avail: Float = 0f): MapLayout {
    val count = ids.size
    var r = if (count <= 3) 180f else min(260f, 130f + count * 14f)
    var padX = 170f // 两侧节点的名称和说明写在外侧，留出文字的宽度
    if (avail > 0 && 2 * (r + padX) > avail) {
        padX = max(110f, avail / 2 - r)
        // 相邻节点的圆心至少隔 72（圆直径 50，外加状态点和间隙）。
        val minR = max(120f, (36 / sin(PI / (count + 1))).toFloat())
        r = max(minR, min(r, avail / 2 - padX))
    }
    val pos = LinkedHashMap<String, MapPoint>()
    pos[MAP_PANEL] = MapPoint(0f, -r)
    ids.forEachIndexed { i, id ->
        val a = -PI / 2 + 2 * PI * (i + 1) / (count + 1)
        pos[id] = MapPoint((r * cos(a)).toFloat(), (r * sin(a)).toFloat())
    }
    val ys = pos.values.map { it.y }
    // 上方还要容得下向外弯的线和它的标签：按每对节点之间可能的线算。
    var top = ys.min() - 64
    val pts = ids.map { pos.getValue(it) }
    for (i in pts.indices) for (j in i + 1 until pts.size) {
        val p = mapBend(pts[i], pts[j], 0.16f)
        top = min(top, (pts[i].y + 2 * p.y + pts[j].y) / 4 - 14)
    }
    val bottom = ys.max() + 64
    val spacing = (2 * r * sin(PI / (count + 1))).toFloat()
    return MapLayout(pos, spacing, MapBox(-r - padX, top, 2 * (r + padX), bottom - top))
}

/** 文字放在圆的外侧，连线都朝内：两侧的节点写在旁边，其余写在上方或下方。 */
fun mapLabelOnSide(at: MapPoint): Boolean = abs(at.x) > 0.7f * hypot(at.x, at.y)

/** 节点名称和说明最宽多少。两侧的写到画布边缘为止；上下方的居中，不超过相邻节点的间距。 */
fun mapLabelWidth(at: MapPoint, box: MapBox, spacing: Float): Float {
    val half = box.w / 2
    if (mapLabelOnSide(at)) return max(48f, half - abs(at.x) - 34 - 4)
    return max(48f, min(spacing - 12, 2 * (half - abs(at.x)) - 8))
}

/** 向外（远离圆心的一侧）弯的控制点，离弦的距离是长度的 [k] 倍。弦正好穿过圆心时向上弯。 */
fun mapBend(a: MapPoint, b: MapPoint, k: Float): MapPoint {
    val m = mid(a, b)
    val dx = b.x - a.x
    val dy = b.y - a.y
    val len = hypot(dx, dy).takeIf { it > 0 } ?: 1f
    var nx = -dy / len
    var ny = dx / len
    val side = nx * m.x + ny * m.y
    if (side < -0.5f || (abs(side) <= 0.5f && ny > 0)) {
        nx = -nx
        ny = -ny
    }
    return MapPoint(m.x + nx * k * len, m.y + ny * k * len)
}

/** 从 [a] 经控制点 [p] 到 [b] 的二次曲线上的点（[s] 从 0 到 1）。 */
fun mapCurveAt(a: MapPoint, b: MapPoint, p: MapPoint, s: Float): MapPoint {
    val t = 1 - s
    return MapPoint(t * t * a.x + 2 * t * s * p.x + s * s * b.x, t * t * a.y + 2 * t * s * p.y + s * s * b.y)
}

/** 有向的线：从 [from] 经 [control] 到箭头尖 [tip] 的二次曲线，[head] 是箭头的三个顶点。 */
data class MapArrow(val from: MapPoint, val control: MapPoint, val tip: MapPoint, val head: List<MapPoint>)

/** 从 [a] 指向 [b] 的曲线，停在 [b] 的外圈外面（半径 31），末端画箭头。 */
fun mapArrow(a: MapPoint, b: MapPoint, p: MapPoint): MapArrow {
    var lo = 0.5f
    var hi = 1f
    repeat(24) {
        val m = (lo + hi) / 2
        if (dist(mapCurveAt(a, b, p, m), b) > 31) lo = m else hi = m
    }
    val tip = mapCurveAt(a, b, p, lo)
    val back = mapCurveAt(a, b, p, lo - 0.03f)
    val len = dist(tip, back).takeIf { it > 0 } ?: 1f
    val ux = (tip.x - back.x) / len
    val uy = (tip.y - back.y) / len
    // 截到箭头尖：de Casteljau，前一段的控制点是 a 和 p 之间的 lo 处。
    val p1 = MapPoint(a.x + (p.x - a.x) * lo, a.y + (p.y - a.y) * lo)
    val head = listOf(
        tip,
        MapPoint(tip.x - ux * 8 - uy * 4, tip.y - uy * 8 + ux * 4),
        MapPoint(tip.x - ux * 8 + uy * 4, tip.y - uy * 8 - ux * 4),
    )
    return MapArrow(a, p1, tip, head)
}

/** 延迟的写法，同网页的 fmtMs：不到 10 ms 保留一位小数，过 1 秒换成秒、过 1 分钟换成分钟。 */
fun formatMs(v: Double): String {
    if (v < 9.95) return String.format(Locale.ROOT, "%.1f ms", v)
    if (v < 999.5) return "${v.roundToLong()} ms"
    val s = v / 1000
    if (s < 59.95) return String.format(Locale.ROOT, if (s < 9.995) "%.2f s" else "%.1f s", s)
    val m = s / 60
    return if (m < 9.95) String.format(Locale.ROOT, "%.1f min", m) else "${m.roundToLong()} min"
}

/** 节点在线、已批准（在节点网络里）。 */
val Node.live: Boolean get() = online && !pending

// ---- 节点网络 ----

enum class MeshKind { DIRECT, RELAY, NONE }

/** 路径从好到差。两端看到的可能不一样（A→B 直连、B→A 中转），取差的那个。 */
fun meshRank(path: String): Int = when (path) {
    "direct", "link" -> 0
    "node-relay", "udp-relay" -> 1
    "tls-relay" -> 2
    else -> 3
}

fun meshKind(path: String): MeshKind = when (meshRank(path)) {
    0 -> MeshKind.DIRECT
    3 -> MeshKind.NONE
    else -> MeshKind.RELAY
}

/**
 * 两端之间的一条连接（无向，[a] < [b]）。[rtt] 是直连时两端测到的延迟的平均，中转时为 0（经面板中转时不测）；
 * [views] 是两端各自看到的对方（键是看的那一端），用来说明没有直连的原因。没有 [views] 的是还没查到状态、
 * 按控制连接补上的面板连线。
 */
data class MeshLink(val a: String, val b: String, val path: String, val kind: MeshKind, val rtt: Double, val views: Map<String, MeshPeerStatus>) {
    val key: String get() = "$a|$b"
    val toPanel: Boolean get() = a == MAP_PANEL || b == MAP_PANEL
    fun touches(id: String?) = id != null && (a == id || b == id)
}

private fun keyHex(b: ByteString) = b.toByteArray().joinToString("") { "%02x".format(it) }

/**
 * 把各端的节点网络状态合成连接。成员按 mesh 地址对应到节点（地址由节点自己的状态得知），非管理员拿不到地址，按公钥对应，
 * 都对不上时按名称。只留 [visible] 里的两端（在线的节点和面板）。在线的节点和面板之间一定有控制连接，还没查到时也补上。
 */
fun buildMeshLinks(nodes: List<Node>, statuses: Map<String, MeshStatus>, panel: MeshStatus?, visible: Set<String>): List<MeshLink> {
    val byAddr = HashMap<String, String>()
    for ((id, s) in statuses) if (s.address.isNotEmpty()) byAddr[s.address] = id
    if (panel != null && panel.address.isNotEmpty()) byAddr[panel.address] = MAP_PANEL
    val byName = nodes.associate { it.name to it.id }
    val byKey = statuses.entries.associate { (id, s) -> keyHex(s.publicKey) to id }

    class Acc(val a: String, val b: String, var path: String, var rtt: Double, val views: LinkedHashMap<String, MeshPeerStatus>)
    val edges = LinkedHashMap<String, Acc>()
    fun add(from: String, to: String?, peer: MeshPeerStatus) {
        if (to == null || from == to || from !in visible || to !in visible) return
        val (a, b) = if (from < to) from to to else to to from
        val rtt = metric(peer.rttMs)
        val old = edges["$a|$b"]
        if (old == null) {
            edges["$a|$b"] = Acc(a, b, peer.path, if (meshRank(peer.path) == 0) rtt else 0.0, linkedMapOf(from to peer))
            return
        }
        old.views[from] = peer
        if (meshRank(peer.path) > meshRank(old.path)) old.path = peer.path
        if (meshKind(old.path) == MeshKind.DIRECT && rtt > 0) old.rtt = if (old.rtt > 0) (old.rtt + rtt) / 2 else rtt
        else if (meshKind(old.path) != MeshKind.DIRECT) old.rtt = 0.0
    }
    fun scan(from: String, s: MeshStatus) {
        for (p in s.peersList) {
            val to = p.address.takeIf { it.isNotEmpty() }?.let { byAddr[it] } ?: byKey[keyHex(p.publicKey)] ?: byName[p.name]
            add(from, to, p)
        }
    }
    for ((id, s) in statuses) scan(id, s)
    if (panel != null) scan(MAP_PANEL, panel)
    if (MAP_PANEL in visible) for (id in visible) {
        if (id == MAP_PANEL) continue
        val (a, b) = if (id < MAP_PANEL) id to MAP_PANEL else MAP_PANEL to id
        if ("$a|$b" !in edges) edges["$a|$b"] = Acc(a, b, "", 0.0, linkedMapOf())
    }
    return edges.values.map { MeshLink(it.a, it.b, it.path, if (it.path.isEmpty()) MeshKind.DIRECT else meshKind(it.path), it.rtt, it.views) }
}

/** 经节点中转时经过的节点名（两端看到的可能不同，取第一个报的）。 */
fun MeshLink.relayVia(): String = views.values.firstOrNull { it.path == "node-relay" && it.via.isNotEmpty() }?.via.orEmpty()

/** 两端看到的延迟里大于 0 的平均（中转时节点有时也报），没有时为 0。 */
fun MeshLink.observedRtt(): Double {
    if (rtt > 0) return rtt
    val v = views.values.map { metric(it.rttMs) }.filter { it > 0 }
    return if (v.isEmpty()) 0.0 else v.average()
}

/** 没有直连的原因，两端是节点 ID。 */
sealed interface RelayReason {
    /** 都在 NAT 后面，[symmetric] 里的按目的地换端口，打洞不会成功。 */
    data class Symmetric(val a: String, val b: String, val symmetric: List<String>) : RelayReason

    data class NoAddress(val node: String) : RelayReason

    /** [from] 收得到 [blocked] 的包，[blocked] 收不到 [from] 的：检查 [blocked] 的 UDP 端口 [port]。 */
    data class OneWay(val blocked: String, val from: String, val port: Int) : RelayReason

    data class NoReply(val a: String, val portA: Int, val b: String, val portB: Int) : RelayReason
}

/**
 * 按两端各自的探测情况说明为什么没有直连，同网页的 relayReason。旧版本的节点不报这些，看不出来时返回 null。
 * 先看 NAT（两端都在 NAT 后面、其中一端按目的地换端口时打洞基本不会成功），再看地址，最后看包是哪个方向过不去。
 */
fun relayReason(l: MeshLink, statuses: Map<String, MeshStatus>): RelayReason? {
    if (l.toPanel) return null
    val a = l.a
    val b = l.b
    val sa = statuses[a]
    val sb = statuses[b]
    fun reported(s: MeshStatus?, v: MeshPeerStatus?) = v != null && (!s?.nat.isNullOrEmpty() || v.tried > 0 || v.heard)
    val va = l.views[a] // a 看到的 b
    val vb = l.views[b] // b 看到的 a
    fun behind(s: MeshStatus?) = s?.nat == "nat" || s?.nat == "symmetric"
    if (behind(sa) && behind(sb) && (sa?.nat == "symmetric" || sb?.nat == "symmetric")) {
        return RelayReason.Symmetric(a, b, listOf(a, b).filter { statuses[it]?.nat == "symmetric" })
    }
    if (reported(sa, va) && va!!.tried == 0 && !va.heard) return RelayReason.NoAddress(b)
    if (reported(sb, vb) && vb!!.tried == 0 && !vb.heard) return RelayReason.NoAddress(a)
    fun port(s: MeshStatus?) = s?.port ?: 0
    // a 收得到 b 的探测、b 收不到 a 的：a 到 b 这个方向过不去。
    if (va?.heard == true && reported(sb, vb) && !vb!!.heard) return RelayReason.OneWay(b, a, port(sb))
    if (vb?.heard == true && reported(sa, va) && !va!!.heard) return RelayReason.OneWay(a, b, port(sa))
    if (reported(sa, va) || reported(sb, vb)) return RelayReason.NoReply(a, port(sa), b, port(sb))
    return null
}

/**
 * 线的控制点：都是直线；只有经面板中转的线经过面板（控制点取中点关于面板的对称点，曲线中点正好在面板上）。
 * 经节点中转的画成直的虚线。
 */
fun meshControl(l: MeshLink, a: MapPoint, b: MapPoint, panel: MapPoint): MapPoint {
    val m = mid(a, b)
    return if (l.viaPanel) MapPoint(2 * panel.x - m.x, 2 * panel.y - m.y) else m
}

/** 两个节点之间经面板中转（线画成经过面板）。 */
val MeshLink.viaPanel: Boolean get() = kind == MeshKind.RELAY && !toPanel && path != "node-relay"

/** 线上标签的位置（曲线参数）：经面板中转的线中点在面板上，标签放在靠近 [active] 的一段。 */
fun meshLabelAt(l: MeshLink, active: String?): Float = if (l.viaPanel) (if (l.a == active) 0.3f else 0.7f) else 0.5f

/**
 * 手机上的列表：先列有问题的（不可达在前、中转在后），再列直连，按延迟从低到高，没测到的放最后。
 * 还没查到状态、补上的面板连线不列。
 */
fun meshListOrder(links: List<MeshLink>): List<MeshLink> {
    val shown = links.filter { it.views.isNotEmpty() }
    val issues = shown.filter { it.kind != MeshKind.DIRECT }.sortedBy { if (it.kind == MeshKind.NONE) 0 else 1 }
    val direct = shown.filter { it.kind == MeshKind.DIRECT }.sortedWith(compareBy({ it.rtt <= 0 }, { it.rtt }))
    return issues + direct
}

/** 延迟矩阵的一格：行所在节点测到列所在节点。 */
data class MeshCell(val path: String, val rtt: Double)

/** 节点两两之间的当前延迟，键是「看的一端|对方」（来自各节点的状态）。 */
fun meshCells(statuses: Map<String, MeshStatus>): Map<String, MeshCell> {
    val byAddr = HashMap<String, String>()
    for ((id, s) in statuses) if (s.address.isNotEmpty()) byAddr[s.address] = id
    val byKey = statuses.entries.associate { (id, s) -> keyHex(s.publicKey) to id }
    val cells = HashMap<String, MeshCell>()
    for ((id, s) in statuses) for (p in s.peersList) {
        val to = p.address.takeIf { it.isNotEmpty() }?.let { byAddr[it] } ?: byKey[keyHex(p.publicKey)] ?: continue
        cells["$id|$to"] = MeshCell(p.path, if (p.path == "direct") metric(p.rttMs) else 0.0)
    }
    return cells
}

/** 一次查到的节点网络状态。[failed] 个节点（或面板）查不到又没有上次的，[error] 是其中第一个的原因。 */
data class MeshSnapshot(val statuses: Map<String, MeshStatus>, val panel: MeshStatus?, val failed: Int = 0, val error: String? = null)

/**
 * 一条连接两个方向的延迟历史：[times] 是两条线共用的时间（秒），[ab] 是 a 测到 b，[ba] 是 b 测到 a（面板不存监控，
 * 一端是面板时只有一个方向，另一条为空）。
 */
data class LatencyHistory(val times: List<Long>, val ab: List<Double>, val ba: List<Double>)

/**
 * 两条序列按时间对齐：两边都有时取时间的并集，缺的一点用前一个值补（开头缺的用后一个值），一边为空时只用另一边的时间。
 * 两端按同样的间隔聚合，时间通常是对齐的，补的只是偶尔少的一两个点。
 */
fun alignLatency(a: Pair<List<Long>, List<Double>>, b: Pair<List<Long>, List<Double>>): LatencyHistory {
    if (a.first.isEmpty()) return LatencyHistory(b.first, emptyList(), b.second)
    if (b.first.isEmpty()) return LatencyHistory(a.first, a.second, emptyList())
    val times = (a.first + b.first).toSortedSet().toList()
    fun fill(s: Pair<List<Long>, List<Double>>): List<Double> {
        val at = s.first.zip(s.second).toMap()
        val out = ArrayList<Double>(times.size)
        var last: Double? = null
        for (t in times) {
            val v = at[t] ?: last
            out += v ?: Double.NaN
            if (v != null) last = v
        }
        val first = out.firstOrNull { !it.isNaN() } ?: 0.0
        return out.map { if (it.isNaN()) first else it }
    }
    return LatencyHistory(times, fill(a), fill(b))
}

// ---- 高可用 ----

/** 一个部署过的容器和它所在的项目。 */
data class HaItem(val projectId: String, val projectName: String, val container: Container)

/** 只看部署过的容器：没部署过的没有在运行，也没有副本。 */
fun haItems(projects: List<ProjectSummary>): List<HaItem> = projects.flatMap { s ->
    if (!s.hasProject()) emptyList()
    else s.project.containersList.filter { it.deployedVersion > 0 }.map { HaItem(s.project.id, s.project.name, it) }
}

/** 顶替的另一方（热备）：A、B 里不是容器当前所在节点的那个。没有设置顶替时为 null。 */
fun Container.standby(): FailoverParticipant? {
    if (!hasFailover()) return null
    return if (failover.a.nodeId == nodeId) failover.b else failover.a
}

/** 有状态、但没有顶替也没有备份节点的容器，所在节点坏了数据就找不回来。 */
val Container.unprotected: Boolean get() = stateful && !hasFailover() && backupNodeIdsCount == 0

enum class HaEdgeType { FAILOVER, BACKUP }

/** 容器所在节点指向它的热备或备份节点，[count] 是容器数。 */
data class HaEdge(val from: String, val to: String, val type: HaEdgeType, val count: Int) {
    val key: String get() = "$from|$to|$type"
    fun touches(id: String?) = id != null && (from == id || to == id)
}

/** 高可用图的箭头，只留两端都在 [nodeIds] 里的。 */
fun haEdges(items: List<HaItem>, nodeIds: Set<String>): List<HaEdge> {
    val counts = LinkedHashMap<Triple<String, String, HaEdgeType>, Int>()
    fun add(from: String, to: String?, type: HaEdgeType) {
        if (to.isNullOrEmpty() || from == to) return
        val k = Triple(from, to, type)
        counts[k] = (counts[k] ?: 0) + 1
    }
    for ((_, _, c) in items) {
        if (c.hasFailover()) add(c.nodeId, c.standby()?.nodeId, HaEdgeType.FAILOVER)
        for (id in c.backupNodeIdsList) add(c.nodeId, id, HaEdgeType.BACKUP)
    }
    return counts.map { (k, n) -> HaEdge(k.first, k.second, k.third, n) }.filter { it.from in nodeIds && it.to in nodeIds }
}

/** 每个节点上无副本的容器数（[unprotected]）。 */
fun haUnprotected(items: List<HaItem>): Map<String, Int> =
    items.filter { it.container.unprotected }.groupingBy { it.container.nodeId }.eachCount()

/** 同一对节点之间几条平行的线相隔多远。 */
private const val HA_LANE_GAP = 10f

/** 一条箭头的形状和线上标签的位置。 */
data class HaEdgeShape(val arrow: MapArrow, val label: MapPoint)

/**
 * 箭头都是直线。同一对节点之间有几条（互相指向，或者同一方向既有顶替又有备份）时平行错开，标签沿线错开。
 * 法线按 ID 小的一端指向大的一端算，两个方向的线错开的方向一致。
 */
fun haShapes(edges: List<HaEdge>, pos: Map<String, MapPoint>): Map<String, HaEdgeShape> {
    fun typeName(t: HaEdgeType) = if (t == HaEdgeType.FAILOVER) "failover" else "backup"
    val lanes = edges.groupBy { listOf(it.from, it.to).sorted().joinToString("|") }
        .mapValues { (_, l) -> l.sortedBy { it.from + typeName(it.type) } }
    val out = HashMap<String, HaEdgeShape>()
    for (e in edges) {
        val a = pos[e.from] ?: continue
        val b = pos[e.to] ?: continue
        val (lo, hi) = listOf(e.from, e.to).sorted()
        val list = lanes.getValue("$lo|$hi")
        val l = pos.getValue(lo)
        val h = pos.getValue(hi)
        val len = dist(l, h).takeIf { it > 0 } ?: 1f
        val offset = list.indexOf(e) - (list.size - 1) / 2f
        val shift = offset * HA_LANE_GAP
        val nx = -(h.y - l.y) / len * shift
        val ny = (h.x - l.x) / len * shift
        val a2 = MapPoint(a.x + nx, a.y + ny)
        val b2 = MapPoint(b.x + nx, b.y + ny)
        val c = mid(a2, b2)
        // 线的方向不同，参数从不同的一端算；按 ID 小的一端换算，错开的顺序才一致。
        val s = 0.5f + offset * 0.2f
        out[e.key] = HaEdgeShape(mapArrow(a2, b2, c), mapCurveAt(a2, b2, c, if (e.from == lo) s else 1 - s))
    }
    return out
}

/** 故障时的结果分组，最严重的在前。 */
enum class HaGroup { LOST, CONFIRM, BACKUP, AUTO, REDEPLOY, COLLATERAL }

/** 一个容器在节点故障时会怎样（按 docs/failover.md），节点都是 ID。[order] 是网页上的排序，组里按它排。 */
sealed class HaFate(val group: HaGroup, val order: Int) {
    data object Lost : HaFate(HaGroup.LOST, 0)
    data class StandbyOffline(val node: String) : HaFate(HaGroup.LOST, 1)
    data class WitnessOffline(val node: String) : HaFate(HaGroup.LOST, 1)
    data class BackupOffline(val nodes: List<String>) : HaFate(HaGroup.LOST, 2)
    data class Confirm(val node: String) : HaFate(HaGroup.CONFIRM, 3)
    data class Backup(val nodes: List<String>) : HaFate(HaGroup.BACKUP, 4)
    data class Auto(val node: String) : HaFate(HaGroup.AUTO, 5)
    data object Redeploy : HaFate(HaGroup.REDEPLOY, 6)

    /** 在别的节点（[runsOn]）上照常运行，但故障的节点是它的热备（[failover]）或备份节点（[backup]）。 */
    data class Collateral(val runsOn: String, val failover: Boolean, val backup: Boolean) : HaFate(HaGroup.COLLATERAL, 7)
}

/**
 * 容器所在的节点故障时会怎样，同网页的 fateOf。顶替要热备和见证人都在：A 坏了，B 加见证人才是多数。
 * 有多个备份节点时，在线的任一个都能用来恢复；顶替因热备或见证人离线做不了时，退而在备份节点上手动恢复。
 */
fun haFate(c: Container, online: (String) -> Boolean): HaFate {
    val sb = c.standby()
    val backups = c.backupNodeIdsList.filter(online)
    if (c.hasFailover() && sb != null && sb.nodeId.isNotEmpty()) {
        val w = c.failover.witness.nodeId
        val witnessDown = !c.failover.witnessIsPanel && w.isNotEmpty() && !online(w)
        val blocked = !online(sb.nodeId) || witnessDown
        if (blocked && backups.isNotEmpty()) return HaFate.Backup(backups)
        if (!online(sb.nodeId)) return HaFate.StandbyOffline(sb.nodeId)
        if (witnessDown) return HaFate.WitnessOffline(w)
        if (c.stateful && !c.failover.autoStateful) return HaFate.Confirm(sb.nodeId)
        return HaFate.Auto(sb.nodeId)
    }
    if (c.backupNodeIdsCount > 0) {
        if (backups.isEmpty()) return HaFate.BackupOffline(c.backupNodeIdsList)
        return HaFate.Backup(backups)
    }
    if (c.stateful) return HaFate.Lost
    return HaFate.Redeploy
}

data class HaRow(val item: HaItem, val fate: HaFate)

/**
 * 模拟节点 [nodeId] 故障：它上面的容器按结果分组，最严重的在前；另外列出把它当作热备或备份节点的容器（失去副本）。
 * 在线按现在的情况算，故障的节点算离线。
 */
fun simulateFailure(nodeId: String, nodes: List<Node>, items: List<HaItem>): List<Pair<HaGroup, List<HaRow>>> {
    val live = nodes.filter { it.live && it.id != nodeId }.map { it.id }.toSet()
    val online = { id: String -> id in live }
    val rows = ArrayList<HaRow>()
    for (it in items) {
        val c = it.container
        if (c.nodeId == nodeId) rows += HaRow(it, haFate(c, online))
        else {
            val failover = c.standby()?.nodeId == nodeId
            val backup = nodeId in c.backupNodeIdsList
            if (failover || backup) rows += HaRow(it, HaFate.Collateral(c.nodeId, failover, backup))
        }
    }
    return rows.sortedBy { it.fate.order }.groupBy { it.fate.group }.toSortedMap().map { (g, r) -> g to r }
}

// ---- 接口 ----

private fun ago(seconds: Long): Timestamp = Timestamp.newBuilder().setSeconds(System.currentTimeMillis() / 1000 - seconds).build()

/**
 * 有过数据以后，重新开始查（节点增减）时的 Loading 和查不到时的 Failed 换成上一次的数据，页面不闪回骨架。
 */
fun <T> Flow<Polled<T>>.keepLast(): Flow<Polled<T>> = flow {
    var last: Polled.Ready<T>? = null
    collect { v ->
        val l = last
        when {
            v is Polled.Ready -> { last = v; emit(v) }
            l == null -> emit(v)
            v is Polled.Failed -> emit(l.copy(live = false, error = v.message))
            else -> emit(l)
        }
    }
}

/** 节点网络、高可用用到的接口（都是网页也在用的），刷新间隔同网页：都是 10 秒。 */
class MapRepo(private val session: PanelSession) {
    private val last = HashMap<String, MeshStatus>()
    private var lastPanel: MeshStatus? = null

    /**
     * 在线节点（[live]）各自的节点网络状态，同时问；面板自己的只有管理员能查（[admin]）。查不到的一端用上次的，
     * 没有上次的才算失败（同网页：有旧数据时偶尔失败一次不提示）；全都查不到时抛出第一个错误。
     */
    suspend fun mesh(live: List<String>, admin: Boolean): MeshSnapshot = coroutineScope {
        val jobs = live.map { id -> async { id to attempt { session.rpc { c, h -> c.nodes.getNodeMesh(GetNodeMeshRequest.newBuilder().setNodeId(id).build(), h) } } } }
        val panelJob = if (admin) async { attempt { session.rpc { c, h -> c.nodes.getPanelMesh(GetPanelMeshRequest.getDefaultInstance(), h) } } } else null
        val results = jobs.awaitAll()
        val errors = ArrayList<PanelException>()
        for ((id, r) in results) {
            r.onSuccess { last[id] = it }.onFailure { if (id !in last) errors += it.toPanelException() }
        }
        last.keys.retainAll(live.toSet())
        val p = panelJob?.await()
        p?.onSuccess { lastPanel = it }?.onFailure { if (lastPanel == null) errors += it.toPanelException() }
        if (!admin) lastPanel = null
        val first = errors.firstOrNull()
        if (first is DeviceRemovedException || first is UnlockRequiredException) throw first
        if (first != null && last.isEmpty() && lastPanel == null) throw first
        MeshSnapshot(live.mapNotNull { id -> last[id]?.let { id to it } }.toMap(), lastPanel, errors.size, first?.message)
    }

    /**
     * 一条连接两个方向的延迟历史（节点存的 m:<对方公钥>:rtt，见 docs/monitoring.md），各自从测量的那一端查。
     * [a]、[b] 是两端的节点 ID，面板不存监控，传 null；[keyA]、[keyB] 是两端的 mesh 公钥。
     */
    suspend fun latency(a: String?, keyA: ByteString, b: String?, keyB: ByteString, range: TrafficRange): LatencyHistory = coroutineScope {
        suspend fun one(node: String?, peer: ByteString): Pair<List<Long>, List<Double>> {
            if (node == null || peer.isEmpty) return emptyList<Long>() to emptyList()
            val name = "m:${keyHex(peer)}:rtt"
            val res = session.rpc { c, h ->
                c.nodes.queryNodeMetrics(QueryNodeMetricsRequest.newBuilder().setNodeId(node).setPrefix(name).setFrom(ago(range.seconds)).setStep(range.step).build(), h)
            }
            val s = res.seriesList.firstOrNull { it.name == name } ?: return emptyList<Long>() to emptyList()
            val n = minOf(s.timesCount, s.valuesCount)
            return s.timesList.take(n) to s.valuesList.take(n).map { metric(it) }
        }
        val ab = async { one(a, keyB) }
        val ba = async { one(b, keyA) }
        alignLatency(ab.await(), ba.await())
    }

    /** 部署过的容器（来自项目列表，要「查看」）。没有权限时是空的。 */
    suspend fun haItems(): List<HaItem> {
        if (!session.access().can(Access.PROJECT_VIEW)) return emptyList()
        return try {
            haItems(session.rpc { c, h -> c.projects.listProjects(ListProjectsRequest.getDefaultInstance(), h) }.projectsList)
        } catch (e: PanelException) {
            if (e.code == Code.PERMISSION_DENIED) emptyList() else throw e
        }
    }

    private suspend fun <T> attempt(block: suspend () -> T): Result<T> = try {
        Result.success(block())
    } catch (e: CancellationException) {
        throw e
    } catch (e: Throwable) {
        Result.failure(e)
    }
}
