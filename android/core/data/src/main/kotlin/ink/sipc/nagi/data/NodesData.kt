package ink.sipc.nagi.data

import com.connectrpc.Code
import com.google.protobuf.Timestamp
import ink.sipc.nagi.network.DeviceRemovedException
import ink.sipc.nagi.network.PanelException
import ink.sipc.nagi.network.UnlockRequiredException
import ink.sipc.nagi.network.toPanelException
import ink.sipc.nagi.proto.v1.CreateEnrollCommandRequest
import ink.sipc.nagi.proto.v1.DeleteImagesResponse
import ink.sipc.nagi.proto.v1.DeleteNodeImagesRequest
import ink.sipc.nagi.proto.v1.GetSettingsRequest
import ink.sipc.nagi.proto.v1.ImagePullEvent
import ink.sipc.nagi.proto.v1.ImageRef
import ink.sipc.nagi.proto.v1.ListNodesRequest
import ink.sipc.nagi.proto.v1.ListProjectsRequest
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.proto.v1.NodeImages
import ink.sipc.nagi.proto.v1.NodeRef
import ink.sipc.nagi.proto.v1.PanelSettings
import ink.sipc.nagi.proto.v1.ProjectSummary
import ink.sipc.nagi.proto.v1.PruneImagesResponse
import ink.sipc.nagi.proto.v1.PruneNodeImagesRequest
import ink.sipc.nagi.proto.v1.PullNodeImageRequest
import ink.sipc.nagi.proto.v1.QueryNodeMetricsRequest
import ink.sipc.nagi.proto.v1.RemoveNodeRequest
import ink.sipc.nagi.proto.v1.RenameNodeRequest
import ink.sipc.nagi.proto.v1.UpdateSettingsRequest
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.channelFlow
import kotlinx.coroutines.selects.onTimeout
import kotlinx.coroutines.selects.select
import kotlinx.coroutines.channels.ReceiveChannel

// 节点页的数据（见 docs/android.md#项目和容器「节点列表」「节点页」）。和网页 pages/nodes.tsx、node-images.tsx 查同样的接口，
// 刷新间隔也一样：节点列表、项目列表 5 秒，监控 10 秒。

/** 一次定时查询的状态。 */
sealed interface Polled<out T> {
    data object Loading : Polled<Nothing>

    /** live 为 false 时连不上面板，显示的是上次查到的，error 是原因。 */
    data class Ready<T>(val value: T, val live: Boolean = true, val error: String? = null) : Polled<T>

    /** 还没有数据就出错了。 */
    data class Failed(val message: String, val unreachable: Boolean) : Polled<Nothing>
}

/**
 * 每 [intervalMs] 查一次，收集的人走了就停。查不到时退避重试（1、2、5、10 秒，之后每 30 秒）；有数据时先静默重试，
 * 连续失败超过 5 秒才标成连不上（和首页一样，见 docs/android.md#数据刷新）。[refresh] 收到时马上再查一次。
 * 要解锁、手机已退出时停下：首页的数据流会处理（锁定、回到欢迎页）。
 */
fun <T> poll(intervalMs: Long, refresh: ReceiveChannel<Unit>? = null, load: suspend () -> T): Flow<Polled<T>> = channelFlow {
    var last: Polled.Ready<T>? = null
    var attempt = 0
    var failingSince = 0L
    send(Polled.Loading)
    while (true) {
        var error: PanelException? = null
        try {
            val v = Polled.Ready(load())
            last = v
            attempt = 0
            failingSince = 0L
            send(v)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Throwable) {
            error = e.toPanelException()
        }
        if (error is DeviceRemovedException || error is UnlockRequiredException) {
            if (last == null) send(Polled.Failed(error.message.orEmpty(), false))
            return@channelFlow
        }
        if (error != null) {
            if (failingSince == 0L) failingSince = System.currentTimeMillis()
            val l = last
            if (l == null) send(Polled.Failed(error.message.orEmpty(), error.code == Code.UNAVAILABLE))
            else if (System.currentTimeMillis() - failingSince >= 5_000) send(l.copy(live = false, error = error.message))
        }
        val wait = if (error == null) intervalMs else POLL_BACKOFF_MS.getOrElse(attempt) { POLL_BACKOFF_MS.last() }
        if (error != null) attempt++
        if (refresh == null) {
            delay(wait)
        } else {
            select {
                refresh.onReceive { attempt = 0 }
                onTimeout(wait) {}
            }
        }
    }
}

private val POLL_BACKOFF_MS = longArrayOf(1_000, 2_000, 5_000, 10_000, 30_000)

/**
 * 节点的占用，和网页的 nodeUsage 一样：CPU 按核数折成 0–1（[cpuPercent] 是节点报的原值，100 是一个核）；
 * 内存、磁盘是 0–1，磁盘是所有盘加起来（旧版本的节点只报数据目录所在的盘）。总量不知道时为 0。
 * 节点报的数原样经面板转发，一律先过 [metric]（被攻破的节点能填 NaN、负数）。
 */
data class NodeUsageInfo(
    val cpuPercent: Double,
    val cpu: Double,
    val memory: Double,
    val memoryTotal: Double,
    val disk: Double,
    val diskUsed: Double,
    val diskTotal: Double,
) {
    /** 三项里最高的一项（排序用）。 */
    val max: Double get() = maxOf(cpu, memory, disk)
}

/** 节点在线、已批准时的占用，否则为 null（离线、待批准的不画占用条）。 */
fun Node.usageInfo(): NodeUsageInfo? {
    if (!online || pending || !hasUsage()) return null
    val cpus = info.cpus
    val raw = metric(usage.cpu)
    val diskSum = usage.disksList.sumOf { metric(it.totalBytes.toDouble()) }
    val diskUsedSum = usage.disksList.sumOf { metric(it.usedBytes.toDouble()) }
    val diskTotal = if (diskSum > 0) diskSum else metric(info.diskBytes.toDouble())
    val disk = if (diskSum > 0) diskUsedSum / diskSum else metric(usage.disk) / 100
    return NodeUsageInfo(
        cpuPercent = raw,
        cpu = (if (cpus > 0) raw / (cpus * 100) else raw / 100).coerceIn(0.0, 1.0),
        memory = (metric(usage.memory) / 100).coerceIn(0.0, 1.0),
        memoryTotal = metric(info.memoryBytes.toDouble()),
        disk = disk.coerceIn(0.0, 1.0),
        diskUsed = if (diskSum > 0) diskUsedSum else disk * diskTotal,
        diskTotal = diskTotal,
    )
}

/** 用量到这里算超过阈值（变红），和网页的占用指示一样。 */
const val USAGE_DANGER = 0.9
const val USAGE_WARNING = 0.75

/** 节点列表和自己的权限（决定入口）。 */
data class NodeList(val nodes: List<Node>, val access: Access)

/** 监控的一组序列：名称（node:cpu、node:mem、node:disk、node:net_rx、node:net_tx）→ 时间（秒）和值。 */
data class NodeMetrics(val times: Map<String, List<Long>>, val values: Map<String, List<Double>>)

private fun ago(seconds: Long): Timestamp = Timestamp.newBuilder().setSeconds(System.currentTimeMillis() / 1000 - seconds).build()

/** 节点页用到的接口（都是网页也在用的）。 */
class NodesRepo(val session: PanelSession) {
    /** 节点列表（要「查看节点」）。没有这个权限时是空的，和网页一样不去问。 */
    suspend fun list(): NodeList {
        val access = session.access()
        if (!access.can(Access.NODE_VIEW)) return NodeList(emptyList(), access)
        val nodes = session.rpc { c, h -> c.nodes.listNodes(ListNodesRequest.getDefaultInstance(), h) }.nodesList
        // 顺序和网页的节点页一样，用面板返回的顺序，不再排。
        return NodeList(nodes, access)
    }

    /** 项目列表（节点页的「容器」）。没有「查看」权限时是空的。 */
    suspend fun projects(): List<ProjectSummary> {
        if (!session.access().can(Access.PROJECT_VIEW)) return emptyList()
        return try {
            session.rpc { c, h -> c.projects.listProjects(ListProjectsRequest.getDefaultInstance(), h) }.projectsList
        } catch (e: PanelException) {
            if (e.code == Code.PERMISSION_DENIED) emptyList() else throw e
        }
    }

    /** 节点的监控（前缀 node:），和网页节点页一样。 */
    suspend fun metrics(nodeId: String, range: TrafficRange): NodeMetrics {
        val res = session.rpc { c, h ->
            c.nodes.queryNodeMetrics(QueryNodeMetricsRequest.newBuilder().setNodeId(nodeId).setPrefix("node:").setFrom(ago(range.seconds)).setStep(range.step).build(), h)
        }
        val times = HashMap<String, List<Long>>()
        val values = HashMap<String, List<Double>>()
        for (s in res.seriesList) {
            val n = minOf(s.timesCount, s.valuesCount)
            times[s.name] = s.timesList.take(n)
            values[s.name] = s.valuesList.take(n).map { metric(it) }
        }
        return NodeMetrics(times, values)
    }

    suspend fun approve(nodeId: String) = session.approveNode(nodeId)

    suspend fun rename(nodeId: String, name: String) {
        session.rpc { c, h -> c.nodes.renameNode(RenameNodeRequest.newBuilder().setNodeId(nodeId).setName(name).build(), h) }
    }

    suspend fun remove(nodeId: String) {
        session.rpc { c, h -> c.nodes.removeNode(RemoveNodeRequest.newBuilder().setNodeId(nodeId).build(), h) }
    }

    // ---- 添加节点（管理员）----

    /** 面板设置：对外地址和端口（生成安装命令前可以改对外地址）。 */
    suspend fun panelSettings(): PanelSettings =
        session.rpc { c, h -> c.settings.getSettings(GetSettingsRequest.getDefaultInstance(), h) }.settings

    /** 生成安装命令。[address] 和现在的对外地址不同时先保存：安装命令用的是保存过的地址。 */
    suspend fun enrollCommand(address: String?): String {
        if (address != null) {
            session.rpc { c, h -> c.settings.updateSettings(UpdateSettingsRequest.newBuilder().setPublicAddress(address).build(), h) }
        }
        return session.rpc { c, h -> c.nodes.createEnrollCommand(CreateEnrollCommandRequest.getDefaultInstance(), h) }.command
    }

    // ---- 镜像（要「节点镜像」，见 docs/agent.md#节点镜像）----

    suspend fun images(nodeId: String): NodeImages =
        session.rpc { c, h -> c.nodes.listNodeImages(NodeRef.newBuilder().setNodeId(nodeId).build(), h) }

    /** 删除镜像或标签。删整个镜像时带上这时看到的标签，之后新加的标签节点不跟着删。 */
    suspend fun deleteImages(nodeId: String, refs: List<ImageRef>): DeleteImagesResponse =
        session.rpc { c, h -> c.nodes.deleteNodeImages(DeleteNodeImagesRequest.newBuilder().setNodeId(nodeId).addAllImages(refs).build(), h) }

    suspend fun pruneImages(nodeId: String, ids: List<String>, buildCache: Boolean): PruneImagesResponse =
        session.rpc { c, h -> c.nodes.pruneNodeImages(PruneNodeImagesRequest.newBuilder().setNodeId(nodeId).addAllIds(ids).setBuildCache(buildCache).build(), h) }

    /** 拉取镜像，进度交给 [onEvent]。取消协程时断开请求，节点随之停止拉取。 */
    suspend fun pullImage(nodeId: String, image: String, onEvent: (ImagePullEvent) -> Unit) {
        session.tokens.call { h ->
            val stream = session.client.nodes.pullNodeImage(h)
            try {
                stream.sendAndClose(PullNodeImageRequest.newBuilder().setNodeId(nodeId).setImage(image).build()).getOrThrow()
                for (ev in stream.responseChannel()) onEvent(ev)
            } finally {
                if (!stream.isReceiveClosed()) stream.receiveClose()
            }
        }
    }

    companion object {
        fun wholeImage(id: String, tags: List<String>): ImageRef =
            ImageRef.newBuilder().setId(id).addAllTags(tags).setCheckTags(true).build()

        fun tag(id: String, tag: String): ImageRef = ImageRef.newBuilder().setId(id).setTag(tag).build()
    }
}
