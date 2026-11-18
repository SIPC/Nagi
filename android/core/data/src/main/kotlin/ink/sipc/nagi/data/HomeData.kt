package ink.sipc.nagi.data

import com.google.protobuf.Timestamp
import ink.sipc.nagi.proto.v1.GetNodeMeshRequest
import ink.sipc.nagi.proto.v1.GetPanelMeshRequest
import ink.sipc.nagi.proto.v1.ListNodesRequest
import ink.sipc.nagi.proto.v1.ListProjectsRequest
import ink.sipc.nagi.proto.v1.MetricSeries
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.proto.v1.ProjectSummary
import ink.sipc.nagi.proto.v1.QueryNodeMetricsRequest
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.withTimeoutOrNull
import org.json.JSONArray
import org.json.JSONObject

// 首页的数据（见 docs/android.md#首页）。和网页的全局概览一样，只用网页也在用的接口，在手机上算
// （web/src/pages/overview.tsx，算法相同）：节点列表、项目列表、每个在线节点最近 1 分钟的网卡、节点网络和端口映射速率，
// 以及节点网络的连接（公钥对应到节点，哪些经面板中转）。对外 = 网卡 − 节点网络（不小于 0），是估算值。

/** 需要处理的一项。标题由界面按 [kind] 和名称拼出来（各语言的词条在首页模块）。 */
data class Attention(
    /** node_pending、node_offline、container_unhealthy、container_dead、image_update。 */
    val kind: String,
    /** critical、warning、info。 */
    val severity: String,
    val nodeId: String = "",
    val nodeName: String = "",
    val projectId: String = "",
    val projectName: String = "",
    val container: String = "",
    /** 第二行（容器所在的节点、镜像）。 */
    val detail: String = "",
    /** 从什么时候开始（秒），不知道时为 0。 */
    val since: Long = 0,
    /** 行尾能一步做完的操作：approve、update，没有时为空。 */
    val action: String = "",
)

/** 一条端口映射最近 24 小时的流量合计（字节），和网页概览的「流量最大的端口映射」一样。 */
data class RouteRate(val projectId: String, val projectName: String, val route: String, val address: String, val rx: Double, val tx: Double)

/** 实时流量的一条路径。from、to 是节点 ID，外部网络是 __external，面板（没有自带节点时）是 __panel。kind 是 ext 或 mesh。 */
data class FlowPath(val from: String, val to: String, val rate: Double, val kind: String, val relay: Boolean = false)

/** 桑基图的一行（节点，或者单独一行的面板）。 */
data class FlowRowInfo(val id: String, val name: String, val offline: Boolean)

/**
 * 节点状态的一行（和网页概览的节点表一样）：在线时有占用。cpu、memory 是百分比；memoryTotal、diskUsed、diskTotal 是字节，
 * 不知道时为 0；rx、tx 是网卡当前的速率（字节/秒），问不到时为 null。
 */
data class NodeStat(
    val id: String,
    val name: String,
    /** online、offline、pending。 */
    val status: String,
    val cpu: Double = 0.0,
    val cpus: Int = 0,
    val memory: Double = 0.0,
    val memoryTotal: Double = 0.0,
    val diskUsed: Double = 0.0,
    val diskTotal: Double = 0.0,
    val rx: Double? = null,
    val tx: Double? = null,
    val containers: Int = 0,
)

data class HomeOverview(
    val attention: List<Attention>,
    val nodesOnline: Int,
    val nodesTotal: Int,
    val containersRunning: Int,
    val containersTotal: Int,
    val externalRx: Double,
    val externalTx: Double,
    /** 流量查到了（至少一个在线节点回了数）。 */
    val trafficReady: Boolean,
    val topRoutes: List<RouteRate>,
    val flows: List<FlowPath>,
    val flowRows: List<FlowRowInfo>,
    val nodes: List<NodeStat> = emptyList(),
)

/** 流量趋势：四个数组一一对应（字节/秒）。 */
data class TrafficSeries(val times: List<Long>, val externalRx: List<Double>, val externalTx: List<Double>, val mesh: List<Double>)

/** 问每个节点都超时了。 */
class PanelTimeoutException : Exception("查询节点的监控超时")

internal const val FLOW_EXTERNAL = "__external"
internal const val FLOW_PANEL = "__panel"
private const val TOP_ROUTES = 3
/** 端口映射的合计按 10 分钟聚合（和网页一样），1 分钟查一次。 */
private const val ROUTE_STEP = 600L
private const val ROUTE_EVERY_MS = 60_000L
private val meshSeries = Regex("^m:([0-9a-f]+):(rx|tx)$")
private val relaySeries = Regex("^m:relay:(rx|tx)$")

private fun MetricSeries.latest(): Double? = valuesList.lastOrNull()

private fun ago(seconds: Long): Timestamp = Timestamp.newBuilder().setSeconds(System.currentTimeMillis() / 1000 - seconds).build()

private fun hex(b: com.google.protobuf.ByteString) = b.toByteArray().joinToString("") { "%02x".format(it) }

/** 节点网络的公钥对应：公钥 → 节点 ID；面板的公钥对应到自带节点（同一台机器），没有自带节点时对应到 __panel。 */
internal class MeshKeys(val peers: Map<String, String>, val panel: String?, val relayed: Set<String>) {
    val panelAlone: Boolean get() = panel != null && peers[panel] == FLOW_PANEL
}

/** 首页数据的查询和计算。节点网络的公钥对应 1 分钟查一次，别的每次都查。 */
internal class HomeLoader(private val session: PanelSession) {
    private var keys: MeshKeys? = null
    private var keysAt = 0L
    private var routeTotals: Map<String, Double> = emptyMap()
    private var routeTotalsAt = 0L

    suspend fun load(role: String): HomeOverview = coroutineScope {
        val nodesJob = async { session.rpc { c, h -> c.nodes.listNodes(ListNodesRequest.getDefaultInstance(), h) }.nodesList }
        val projectsJob = async { session.rpc { c, h -> c.projects.listProjects(ListProjectsRequest.getDefaultInstance(), h) }.projectsList }
        val nodes = nodesJob.await()
        val projects = projectsJob.await()
        val online = nodes.filter { !it.pending && it.online }
        val k = meshKeys(online)
        // 每个在线节点最近 1 分钟的三类速率，同时问，单个节点最多等 5 秒；问不到的节点不算。
        val ratesJob = online.map { n -> async { n to nodeRates(n.id) } }
        val totalsJob = async { routeTotals(online) }
        val rates = ratesJob.awaitAll().filter { it.second != null }.associate { it.first.id to it.second!! }
        build(role, nodes, projects, rates, k, totalsJob.await())
    }

    private suspend fun meshKeys(online: List<Node>): MeshKeys = coroutineScope {
        val cached = keys
        if (cached != null && System.currentTimeMillis() - keysAt < 60_000) return@coroutineScope cached
        val statuses = online.map { n -> async { n.id to runCatching { session.rpc { c, h -> c.nodes.getNodeMesh(GetNodeMeshRequest.newBuilder().setNodeId(n.id).build(), h) } }.getOrNull() } }.awaitAll()
        val panel = runCatching { session.rpc { c, h -> c.nodes.getPanelMesh(GetPanelMeshRequest.getDefaultInstance(), h) } }.getOrNull()
        val peers = HashMap<String, String>()
        for ((id, s) in statuses) if (s != null) peers[hex(s.publicKey)] = id
        val relayed = HashSet<String>()
        for ((id, s) in statuses) {
            for (p in s?.peersList.orEmpty()) {
                val to = peers[hex(p.publicKey)] ?: continue
                if (p.path == "udp-relay" || p.path == "tls-relay") { relayed += "$id|$to"; relayed += "$to|$id" }
            }
        }
        var panelKey: String? = null
        if (panel != null) {
            panelKey = hex(panel.publicKey)
            peers[panelKey] = online.firstOrNull { it.ownEmbedded }?.id ?: FLOW_PANEL
        }
        MeshKeys(peers, panelKey, relayed).also { keys = it; keysAt = System.currentTimeMillis() }
    }

    private class NodeRates(val net: List<MetricSeries>, val mesh: List<MetricSeries>)

    /**
     * 端口映射最近 24 小时的流量合计：每个在线节点按 10 分钟聚合的 r: 序列（速率 × 时长），各节点加起来。
     * 1 分钟查一次，其余时候用上次的。问不到的节点不算。
     */
    private suspend fun routeTotals(online: List<Node>): Map<String, Double> = coroutineScope {
        if (System.currentTimeMillis() - routeTotalsAt < ROUTE_EVERY_MS) return@coroutineScope routeTotals
        val parts = online.map { n ->
            async {
                withTimeoutOrNull(15_000) {
                    runCatching {
                        session.rpc { c, h ->
                            c.nodes.queryNodeMetrics(QueryNodeMetricsRequest.newBuilder().setNodeId(n.id).setPrefix("r:").setFrom(ago(86_400)).setStep(ROUTE_STEP).build(), h)
                        }.seriesList
                    }.getOrNull()
                }
            }
        }.awaitAll().filterNotNull()
        val totals = HashMap<String, Double>()
        for (list in parts) for (s in list) totals[s.name] = (totals[s.name] ?: 0.0) + s.valuesList.sumOf { it * ROUTE_STEP }
        totals.also { routeTotals = it; routeTotalsAt = System.currentTimeMillis() }
    }

    private suspend fun nodeRates(nodeId: String): NodeRates? = withTimeoutOrNull(5_000) {
        coroutineScope {
            fun q(prefix: String) = async {
                session.rpc { c, h -> c.nodes.queryNodeMetrics(QueryNodeMetricsRequest.newBuilder().setNodeId(nodeId).setPrefix(prefix).setFrom(ago(60)).build(), h) }.seriesList
            }
            val net = q("node:net_")
            val mesh = q("m:")
            runCatching { NodeRates(net.await(), mesh.await()) }.getOrNull()
        }
    }

    private fun build(role: String, nodes: List<Node>, projects: List<ProjectSummary>, rates: Map<String, NodeRates>, keys: MeshKeys, routeTotals: Map<String, Double>): HomeOverview {
        val nodeById = nodes.associateBy { it.id }
        val offline = nodes.filter { !it.pending && !it.online }.mapTo(HashSet()) { it.id }
        val attention = ArrayList<Attention>()
        for (n in nodes) {
            when {
                n.pending -> attention += Attention("node_pending", "warning", nodeId = n.id, nodeName = n.name, action = if (role == "admin") "approve" else "")
                !n.online -> attention += Attention("node_offline", "critical", nodeId = n.id, nodeName = n.name, since = n.lastSeen.seconds)
            }
        }
        var total = 0
        var running = 0
        for (s in projects) {
            val p = s.project
            for (c in p.containersList) {
                total++
                if (c.nodeId in offline || c.deployedVersion == 0L) continue
                val svcs = s.status.servicesList.filter { it.container == c.name }
                // 离线节点上的容器状态未知，不算运行中，也不算异常（和网页一样）。
                if (svcs.isNotEmpty() && svcs.all { it.state == "running" } && svcs.none { it.health == "unhealthy" || it.health == "starting" }) running++
                val kind = when {
                    svcs.any { it.health == "unhealthy" } -> "container_unhealthy"
                    svcs.any { it.state == "dead" } -> "container_dead"
                    else -> null
                }
                if (kind != null) {
                    attention += Attention(kind, "critical", nodeId = c.nodeId, projectId = p.id, projectName = p.name, container = c.name, detail = nodeById[c.nodeId]?.name.orEmpty())
                }
            }
            for (up in s.status.imageUpdatesList.distinctBy { it.container }) {
                attention += Attention(
                    "image_update", "info", projectId = p.id, projectName = p.name, container = up.container, detail = up.image,
                    action = if (role != "viewer") "update" else "",
                )
            }
        }
        val rank = mapOf("critical" to 0, "warning" to 1, "info" to 2)
        attention.sortWith(compareBy<Attention> { rank[it.severity] ?: 3 }.thenBy { it.since })

        // 流量：对外 = 网卡 − 节点网络。节点网络里自带节点和面板之间走本机回环，不扣。
        val extRx = HashMap<String, Double>()
        val extTx = HashMap<String, Double>()
        val pairs = HashMap<String, Double>()
        for ((id, r) in rates) {
            val embedded = nodeById[id]?.ownEmbedded == true
            var rx = 0.0
            var tx = 0.0
            for (s in r.net) when (s.name) {
                "node:net_rx" -> rx = s.latest() ?: 0.0
                "node:net_tx" -> tx = s.latest() ?: 0.0
            }
            var mrx = 0.0
            var mtx = 0.0
            for (s in r.mesh) {
                val v = s.latest() ?: continue
                val relay = relaySeries.find(s.name)
                val m = meshSeries.find(s.name)
                val dir = when {
                    relay != null -> relay.groupValues[1]
                    m != null && !(embedded && m.groupValues[1] == keys.panel) -> m.groupValues[2]
                    else -> null
                }
                if (dir == "rx") mrx += v else if (dir == "tx") mtx += v
                if (m != null) addPair(pairs, id, m.groupValues[1], m.groupValues[2], v, keys)
            }
            extRx[id] = maxOf(0.0, rx - mrx)
            extTx[id] = maxOf(0.0, tx - mtx)
        }

        // 和网页一样：有请求或有流量的才列，按流量排，一样多时按请求数排。
        val ranked = ArrayList<Pair<RouteRate, Double>>()
        for (s in projects) {
            val p = s.project
            for (r in p.routesList) {
                val key = "r:${p.id}/${r.name}:"
                val rx = routeTotals[key + "in"] ?: 0.0
                val tx = routeTotals[key + "out"] ?: 0.0
                val req = routeTotals[key + "req"] ?: 0.0
                if (req <= 0 && rx + tx <= 0) continue
                val node = p.containersList.firstOrNull { it.name == r.container }?.nodeId?.let { nodeById[it] }
                ranked += RouteRate(p.id, p.name, r.name, routeAddress(r, node), rx, tx) to req
            }
        }
        val routes = ranked.sortedWith(compareByDescending<Pair<RouteRate, Double>> { it.first.rx + it.first.tx }.thenByDescending { it.second }).map { it.first }

        val rows = nodes.filter { !it.pending }.map { FlowRowInfo(it.id, it.name, !it.online) } +
            if (keys.panelAlone) listOf(FlowRowInfo(FLOW_PANEL, "", false)) else emptyList()
        val flows = ArrayList<FlowPath>()
        if (rates.isNotEmpty()) {
            for (r in rows) {
                extRx[r.id]?.takeIf { it >= 1 }?.let { flows += FlowPath(FLOW_EXTERNAL, r.id, it, "ext") }
                extTx[r.id]?.takeIf { it >= 1 }?.let { flows += FlowPath(r.id, FLOW_EXTERNAL, it, "ext") }
            }
            for ((k, v) in pairs.toSortedMap()) {
                if (v < 1) continue
                val (from, to) = k.split("|")
                flows += FlowPath(from, to, v, "mesh", k in keys.relayed)
            }
        }
        val nodeStats = nodes.map { n ->
            val status = if (n.pending) "pending" else if (n.online) "online" else "offline"
            if (!n.online || n.pending) return@map NodeStat(n.id, n.name, status, containers = n.containers)
            val u = n.usage
            val diskTotal = u.disksList.sumOf { it.totalBytes.toDouble() }.takeIf { it > 0 } ?: n.info.diskBytes.toDouble()
            val diskUsed = if (u.disksCount > 0) u.disksList.sumOf { it.usedBytes.toDouble() } else u.disk / 100 * diskTotal
            var rx: Double? = null
            var tx: Double? = null
            rates[n.id]?.net?.forEach { s ->
                when (s.name) {
                    "node:net_rx" -> rx = s.latest()
                    "node:net_tx" -> tx = s.latest()
                }
            }
            NodeStat(n.id, n.name, status, u.cpu, n.info.cpus, u.memory, n.info.memoryBytes.toDouble(), diskUsed, diskTotal, rx, tx, n.containers)
        }
        return HomeOverview(
            attention = attention,
            nodesOnline = nodes.count { !it.pending && it.online },
            nodesTotal = nodes.count { !it.pending },
            containersRunning = running,
            containersTotal = total,
            externalRx = extRx.values.sum(),
            externalTx = extTx.values.sum(),
            trafficReady = rates.isNotEmpty(),
            topRoutes = routes.take(TOP_ROUTES),
            flows = flows,
            flowRows = rows,
            nodes = nodeStats,
        )
    }

    /** 访问地址：绑了域名的写域名，否则是节点地址和端口。 */
    private fun routeAddress(r: ink.sipc.nagi.proto.v1.Exposure, node: Node?): String {
        if (r.domainsCount > 0) return (if (r.httpOnly) "http://" else "https://") + r.getDomains(0)
        val host = node?.address?.ifEmpty { null } ?: session.panel.address.substringBeforeLast(':')
        if (r.published == 0) return host
        return if (host.contains(':')) "[$host]:${r.published}" else "$host:${r.published}"
    }

    /**
     * 流量趋势：访问范围内各在线节点按 [step] 聚合的网卡和节点网络速率，按时刻合计（和网页概览的趋势一样）。
     * 最后一个时刻只有部分节点上报时当作还没有数据（合计会突然变小）。
     */
    suspend fun traffic(seconds: Long, step: Long): TrafficSeries = coroutineScope {
        val nodes = session.rpc { c, h -> c.nodes.listNodes(ListNodesRequest.getDefaultInstance(), h) }.nodesList.filter { !it.pending && it.online }
        val keys = meshKeys(nodes)
        val started = System.currentTimeMillis()
        val errors = java.util.concurrent.atomic.AtomicReference<Throwable?>(null)
        // 历史数据比当前值大，单个节点最多等 15 秒；问不到的节点不计入。
        val parts = nodes.map { n ->
            async {
                withTimeoutOrNull(15_000) {
                    fun q(prefix: String) = async {
                        session.rpc { c, h ->
                            c.nodes.queryNodeMetrics(QueryNodeMetricsRequest.newBuilder().setNodeId(n.id).setPrefix(prefix).setFrom(ago(seconds)).setStep(step).build(), h)
                        }.seriesList
                    }
                    val net = q("node:net_")
                    val mesh = q("m:")
                    runCatching { Triple(n, net.await(), mesh.await()) }.onFailure { errors.compareAndSet(null, it) }.getOrNull()
                }
            }
        }.awaitAll().filterNotNull()
        // 一个节点都没问到：当作出错（显示红字和「重试」），不画空的图。
        if (nodes.isNotEmpty() && parts.isEmpty()) throw errors.get() ?: PanelTimeoutException()
        val rx = HashMap<Long, Double>()
        val tx = HashMap<Long, Double>()
        val sent = HashMap<Long, Double>()
        val count = HashMap<Long, Int>()
        for ((n, net, mesh) in parts) {
            val embedded = n.ownEmbedded
            val drx = HashMap<Long, Double>()
            val dtx = HashMap<Long, Double>()
            for (s in mesh) {
                val relay = relaySeries.find(s.name)
                val m = meshSeries.find(s.name)
                val dir = when {
                    relay != null -> relay.groupValues[1]
                    m != null && !(embedded && m.groupValues[1] == keys.panel) -> m.groupValues[2]
                    else -> continue
                }
                s.timesList.forEachIndexed { i, t ->
                    val v = s.getValues(i)
                    if (dir == "rx") drx[t] = (drx[t] ?: 0.0) + v
                    else {
                        dtx[t] = (dtx[t] ?: 0.0) + v
                        if (m != null) sent[t] = (sent[t] ?: 0.0) + v
                    }
                }
            }
            val seen = HashSet<Long>()
            for (s in net) {
                s.timesList.forEachIndexed { i, t ->
                    val v = s.getValues(i)
                    when (s.name) {
                        "node:net_rx" -> rx[t] = (rx[t] ?: 0.0) + maxOf(0.0, v - (drx[t] ?: 0.0))
                        "node:net_tx" -> tx[t] = (tx[t] ?: 0.0) + maxOf(0.0, v - (dtx[t] ?: 0.0))
                    }
                    if (seen.add(t)) count[t] = (count[t] ?: 0) + 1
                }
            }
        }
        var times = (rx.keys + tx.keys).toSortedSet().toList()
        if (times.size > 1 && (count[times.last()] ?: 0) < (count[times[times.size - 2]] ?: 0)) times = times.dropLast(1)
        TrafficSeries(times, times.map { rx[it] ?: 0.0 }, times.map { tx[it] ?: 0.0 }, times.map { sent[it] ?: 0.0 })
    }
}

/**
 * 节点 node 上的一条 m:<公钥>:rx|tx 记进 pairs（和网页概览的 addRate 一样）：节点之间只取发送方的数；
 * 面板自己不存指标，面板发出的用节点这一端的接收量算；自带节点和面板之间走本机回环，不算。
 */
private fun addPair(pairs: HashMap<String, Double>, node: String, key: String, dir: String, v: Double, keys: MeshKeys) {
    val peer = keys.peers[key] ?: return
    if (peer == node) return
    val k = if (key == keys.panel) {
        if (dir == "tx") "$node|$peer" else "$peer|$node"
    } else {
        if (dir != "tx") return
        "$node|$peer"
    }
    pairs[k] = (pairs[k] ?: 0.0) + v
}

// ---- 缓存（SessionCache 加密存着上一次的首页数据）----

internal fun HomeOverview.toJson(): String = JSONObject().apply {
    put("attention", JSONArray(attention.map {
        JSONObject().put("kind", it.kind).put("severity", it.severity).put("nodeId", it.nodeId).put("nodeName", it.nodeName)
            .put("projectId", it.projectId).put("projectName", it.projectName).put("container", it.container).put("detail", it.detail)
            .put("since", it.since).put("action", it.action)
    }))
    put("nodesOnline", nodesOnline); put("nodesTotal", nodesTotal); put("containersRunning", containersRunning); put("containersTotal", containersTotal)
    put("externalRx", externalRx); put("externalTx", externalTx); put("trafficReady", trafficReady)
    put("topRoutes", JSONArray(topRoutes.map {
        JSONObject().put("projectId", it.projectId).put("projectName", it.projectName).put("route", it.route).put("address", it.address).put("rx", it.rx).put("tx", it.tx)
    }))
    put("flows", JSONArray(flows.map { JSONObject().put("from", it.from).put("to", it.to).put("rate", it.rate).put("kind", it.kind).put("relay", it.relay) }))
    put("flowRows", JSONArray(flowRows.map { JSONObject().put("id", it.id).put("name", it.name).put("offline", it.offline) }))
    put("nodes", JSONArray(nodes.map {
        JSONObject().put("id", it.id).put("name", it.name).put("status", it.status).put("cpu", it.cpu).put("cpus", it.cpus).put("memory", it.memory)
            .put("memoryTotal", it.memoryTotal).put("diskUsed", it.diskUsed).put("diskTotal", it.diskTotal)
            .put("rx", it.rx ?: JSONObject.NULL).put("tx", it.tx ?: JSONObject.NULL).put("containers", it.containers)
    }))
}.toString()

internal fun homeOverviewFromJson(raw: String): HomeOverview {
    val o = JSONObject(raw)
    fun <T> list(name: String, f: (JSONObject) -> T): List<T> {
        val a = o.optJSONArray(name) ?: return emptyList()
        return (0 until a.length()).map { f(a.getJSONObject(it)) }
    }
    return HomeOverview(
        attention = list("attention") {
            Attention(it.getString("kind"), it.getString("severity"), it.optString("nodeId"), it.optString("nodeName"), it.optString("projectId"),
                it.optString("projectName"), it.optString("container"), it.optString("detail"), it.optLong("since"), it.optString("action"))
        },
        nodesOnline = o.optInt("nodesOnline"), nodesTotal = o.optInt("nodesTotal"),
        containersRunning = o.optInt("containersRunning"), containersTotal = o.optInt("containersTotal"),
        externalRx = o.optDouble("externalRx", 0.0), externalTx = o.optDouble("externalTx", 0.0), trafficReady = o.optBoolean("trafficReady"),
        topRoutes = list("topRoutes") { RouteRate(it.optString("projectId"), it.optString("projectName"), it.optString("route"), it.optString("address"), it.optDouble("rx"), it.optDouble("tx")) },
        flows = list("flows") { FlowPath(it.getString("from"), it.getString("to"), it.optDouble("rate"), it.optString("kind"), it.optBoolean("relay")) },
        flowRows = list("flowRows") { FlowRowInfo(it.getString("id"), it.optString("name"), it.optBoolean("offline")) },
        nodes = list("nodes") {
            NodeStat(
                it.getString("id"), it.optString("name"), it.optString("status"), it.optDouble("cpu", 0.0), it.optInt("cpus"), it.optDouble("memory", 0.0),
                it.optDouble("memoryTotal", 0.0), it.optDouble("diskUsed", 0.0), it.optDouble("diskTotal", 0.0),
                if (it.isNull("rx")) null else it.optDouble("rx"), if (it.isNull("tx")) null else it.optDouble("tx"), it.optInt("containers"),
            )
        },
    )
}
