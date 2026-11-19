package ink.sipc.nagi.home

import androidx.compose.foundation.layout.ColumnScope
import ink.sipc.nagi.ui.NagiIcons
import androidx.compose.material3.Icon
import androidx.compose.ui.graphics.vector.ImageVector
import ink.sipc.nagi.ui.barSpec
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.ui.unit.sp
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.geometry.Offset
import androidx.compose.foundation.Canvas
import ink.sipc.nagi.ui.StatusDot
import androidx.compose.ui.text.style.TextOverflow
import ink.sipc.nagi.ui.rowClickable
import ink.sipc.nagi.ui.formatBytesText
import ink.sipc.nagi.data.NodeStat
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.unit.Dp
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.ui.Alignment
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.Box
import androidx.compose.runtime.saveable.rememberSaveable
import ink.sipc.nagi.ui.formatRateText
import ink.sipc.nagi.ui.SANKEY_EXTERNAL
import ink.sipc.nagi.ui.FlowKind
import ink.sipc.nagi.ui.SankeyTexts
import ink.sipc.nagi.ui.SankeyView
import ink.sipc.nagi.ui.SankeyFlow
import ink.sipc.nagi.ui.SankeyRow
import ink.sipc.nagi.ui.SankeyChart
import androidx.compose.runtime.setValue
import androidx.compose.runtime.getValue
import androidx.compose.foundation.layout.RowScope
import ink.sipc.nagi.ui.Banner
import ink.sipc.nagi.ui.UsageRow
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.StatValue
import ink.sipc.nagi.ui.RowDensity
import ink.sipc.nagi.ui.MoreRow
import ink.sipc.nagi.ui.ListRow
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.togetherWith
import ink.sipc.nagi.ui.Collapse
import ink.sipc.nagi.ui.Motion
import ink.sipc.nagi.ui.Roll
import ink.sipc.nagi.ui.autoHeight
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ink.sipc.nagi.data.OverviewState
import ink.sipc.nagi.data.TrafficRange
import ink.sipc.nagi.data.Attention
import ink.sipc.nagi.data.HomeOverview
import ink.sipc.nagi.data.RouteRate
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.ChartLegend
import ink.sipc.nagi.ui.ChartLine
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.LineChart
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SectionDivider
import ink.sipc.nagi.ui.SectionHeader
import ink.sipc.nagi.ui.Skeleton
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.UnderlineTabs
import ink.sipc.nagi.ui.formatRate
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/** 首页「需要处理」最多列几条。 */
private const val ATTENTION_LIMIT = 5

/** 点「需要处理」的一行或行尾按钮时要做的事，交给 App 的导航。 */
interface HomeActions {
    fun openAttention(item: Attention)
    fun openNodes(offlineOnly: Boolean)

    /** 打开一个节点的页面（节点状态里点一个节点）。 */
    fun openNode(id: String)
    fun openProjects(notRunningOnly: Boolean)
    fun update(item: Attention)
}

/**
 * 首页（s1 第 1 张、s8 状态）：需要处理、关键数字、实时流量、流量趋势、流量最大的映射。数据和网页的全局概览一样用网页的接口查（见 HomeData.kt）。
 * 顶部栏和胶囊导航由外层（App）放。[contentPadding] 的底部留出胶囊的 104。
 */
@Composable
fun HomeContent(
    vm: HomeViewModel,
    actions: HomeActions,
    contentPadding: PaddingValues,
    showMessage: (String) -> Unit,
    listState: LazyListState = rememberLazyListState(),
) {
    val state by vm.overview.collectAsStateWithLifecycle()
    val traffic by vm.traffic.collectAsStateWithLifecycle()
    val range by vm.range.collectAsStateWithLifecycle()
    val approving by vm.approving.collectAsStateWithLifecycle()
    LaunchedEffect(vm) { vm.messages.collect(showMessage) }
    val approved = stringResource(R.string.approved)
    var flowView by rememberSaveable { mutableStateOf(SankeyView.All) }

    LazyColumn(Modifier.fillMaxSize(), state = listState, contentPadding = contentPadding) {
        when (val s = state) {
            OverviewState.Loading, OverviewState.Locked, OverviewState.Removed -> item(key = "loading") {
                LoadingBody(Modifier.animateItem(fadeInSpec = null, placementSpec = null, fadeOutSpec = null))
            }
            is OverviewState.Failed -> item(key = "failed") {
                if (s.unreachable) {
                    Unreachable(null, s.message, vm::refresh)
                } else {
                    Column(Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 16.dp)) {
                        Text(s.message, style = NagiTheme.type.body, color = NagiTheme.colors.destructive)
                        Spacer(Modifier.height(12.dp))
                        NagiButton(stringResource(R.string.retry), vm::refresh, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    }
                }
            }
            is OverviewState.Ready -> {
                // 一直占一个位置，出现和消失时高度和透明度一起过渡（Collapse），下面的内容跟着移动。
                item(key = "unreachable") {
                    Collapse(!s.live) { Unreachable(s.updatedAt, s.error.orEmpty(), vm::refresh) }
                }
                item(key = "content") {
                    // 骨架换成内容：淡入 160ms。
                    Column(
                        Modifier
                            .animateItem(fadeInSpec = tween(Motion.FADE_IN, easing = Motion.Enter), placementSpec = null, fadeOutSpec = null)
                            .padding(horizontal = Dimens.pageHorizontal),
                    ) {
                        Spacer(Modifier.height(Dimens.topBarToContent))
                        // 没有要处理的事项时整块隐藏：最后一项处理掉时高度和透明度一起收起（Collapse）。
                        val attention: @Composable () -> Unit = {
                            Collapse(s.overview.attention.isNotEmpty()) {
                                AttentionSection(s.overview, approving, actions) { item ->
                                    vm.approve(item.nodeId, item.nodeName) { approved }
                                }
                                SectionDivider()
                            }
                        }
                        var wideHome by remember { mutableStateOf(false) }
                        BoxWithConstraints {
                            wideHome = maxWidth >= 720.dp && s.overview.flowRows.isNotEmpty()
                            // 宽屏（内容区 720 以上）：需要处理整行在上面；下面左边一栏是关键数字（两行两列）和流量趋势，
                            // 右边是实时流量的桑基图（约 60%）。窄的时候从上到下：需要处理、关键数字、实时流量、流量趋势。
                            if (maxWidth >= 720.dp && s.overview.flowRows.isNotEmpty()) {
                                // 左边一栏的高度：右边的桑基图铺满这个高度（行距放宽或收紧），两边一样高。
                                val density = LocalDensity.current
                                var leftHeight by remember { mutableStateOf(0.dp) }
                                Column {
                                    attention()
                                    Row(verticalAlignment = Alignment.Top) {
                                        Column(Modifier.weight(1f).onSizeChanged { leftHeight = with(density) { it.height.toDp() } }) {
                                            // 和桑基图的标题行对齐：第一个数字的名称和「实时流量」在同一高度。
                                            Spacer(Modifier.height(8.dp))
                                            KeyNumbers(s.overview, actions)
                                            SectionDivider()
                                            TrafficSection(traffic, range, vm::setRange, vm::loadTraffic)
                                            if (s.overview.nodes.isNotEmpty()) {
                                                SectionDivider()
                                                NodesSection(s.overview.nodes, actions, columns = 2)
                                            }
                                            if (s.overview.topRoutes.isNotEmpty()) {
                                                SectionDivider()
                                                TopRoutes(s.overview.topRoutes)
                                            }
                                        }
                                        Spacer(Modifier.width(40.dp))
                                        // 右边一栏和左边一样高，桑基图铺满标题行以下的空间，底部和左边对齐。
                                        Column(Modifier.weight(1.6f).then(if (leftHeight > 200.dp) Modifier.height(leftHeight) else Modifier)) {
                                            FlowSection(s.overview, flowView, fill = leftHeight > 200.dp) { flowView = it }
                                        }
                                    }
                                }
                            } else {
                                Column {
                                    attention()
                                    KeyNumbers(s.overview, actions)
                                    if (s.overview.flowRows.isNotEmpty()) {
                                        SectionDivider()
                                        FlowSection(s.overview, flowView) { flowView = it }
                                    }
                                    SectionDivider()
                                    TrafficSection(traffic, range, vm::setRange, vm::loadTraffic)
                                    if (s.overview.nodes.isNotEmpty()) {
                                        SectionDivider()
                                        NodesSection(s.overview.nodes, actions)
                                    }
                                }
                            }
                        }
                        if (s.overview.topRoutes.isNotEmpty() && !wideHome) {
                            SectionDivider()
                            TopRoutes(s.overview.topRoutes)
                        }
                    }
                }
            }
        }
    }
}

// ---- 需要处理 ----

@Composable
private fun AttentionSection(o: HomeOverview, approving: Set<String>, actions: HomeActions, approve: (Attention) -> Unit) {
    // 收起的过程中还显示最后的那些事项，不闪成空的。
    val last = remember { arrayOf(o.attention) }
    if (o.attention.isNotEmpty()) last[0] = o.attention
    val items = last[0]
    var expanded by remember { mutableStateOf(false) }
    SectionHeader(stringResource(R.string.attention_title)) {
        if (items.isNotEmpty()) Roll(items.size.toString(), NagiTheme.type.secondary, NagiTheme.colors.mutedForeground)
    }
    // 条目增减时过渡到新高度（AutoHeight），下面的内容跟着移动。
    Column(Modifier.fillMaxWidth().autoHeight()) { AttentionBody(items, expanded, { expanded = true }, approving, actions, approve) }
}

@Composable
private fun AttentionBody(
    items: List<Attention>,
    expanded: Boolean,
    expand: () -> Unit,
    approving: Set<String>,
    actions: HomeActions,
    approve: (Attention) -> Unit,
) {
    val shown = if (expanded || items.size <= ATTENTION_LIMIT) items else items.take(ATTENTION_LIMIT - 1)
    shown.forEach { AttentionRow(it, it.nodeId in approving, actions, approve) }
    if (shown.size < items.size) {
        MoreRow(stringResource(R.string.attention_more, items.size - shown.size), expand, density = RowDensity.Compact)
    }
}

@Composable
private fun AttentionRow(item: Attention, busy: Boolean, actions: HomeActions, approve: (Attention) -> Unit) {
    val colors = NagiTheme.colors
    val dot = when (item.severity) {
        "critical" -> colors.destructive
        "warning" -> colors.warning
        else -> colors.info
    }
    val trailing: (@Composable RowScope.() -> Unit)? = when (item.action) {
        "approve" -> {
            { NagiButton(stringResource(R.string.action_approve), { approve(item) }, variant = ButtonVariant.Outline, size = ButtonSize.Small, loading = busy) }
        }
        "update" -> {
            { NagiButton(stringResource(R.string.action_update), { actions.update(item) }, variant = ButtonVariant.Outline, size = ButtonSize.Small) }
        }
        else -> null
    }
    ListRow(
        attentionTitle(item), detail = attentionDetail(item), dot = dot, density = RowDensity.Compact,
        onClick = { actions.openAttention(item) }, trailing = trailing,
    )
}

/** 标题：节点 node7 等待批准、web / api 健康检查失败……（和网页的说法一样）。 */
@Composable
private fun attentionTitle(item: Attention): String = when (item.kind) {
    "node_pending" -> stringResource(R.string.att_node_pending, item.nodeName)
    "node_offline" -> stringResource(R.string.att_node_offline, item.nodeName)
    "container_unhealthy" -> stringResource(R.string.att_container_unhealthy, item.projectName, item.container)
    "container_dead" -> stringResource(R.string.att_container_dead, item.projectName, item.container)
    "image_update" -> stringResource(R.string.att_image_update, item.projectName, item.container)
    else -> item.detail
}

/** 第二行：对象和时长，例如「已离线 4 分钟」「node1 · 12 分钟」。 */
@Composable
private fun attentionDetail(item: Attention): String {
    val since = if (item.since > 0) durationText(item.since) else ""
    return when (item.kind) {
        "node_offline" -> if (since.isNotEmpty()) stringResource(R.string.offline_for, since) else ""
        "node_pending" -> ""
        else -> listOf(item.detail, since).filter { it.isNotEmpty() }.joinToString(" · ")
    }
}

/** 距现在多久：4 分钟、2 小时、3 天。 */
@Composable
private fun durationText(epochSeconds: Long): String {
    val d = (System.currentTimeMillis() / 1000 - epochSeconds).coerceAtLeast(0)
    return when {
        d < 3600 -> stringResource(R.string.minutes, (d / 60).coerceAtLeast(1))
        d < 86400 -> stringResource(R.string.hours, d / 3600)
        else -> stringResource(R.string.days, d / 86400)
    }
}

// ---- 关键数字 ----

/** 关键数字：手机上两行两列；[vertical] 时（宽屏，放在桑基图左边）四个竖着排。 */
@Composable
private fun KeyNumbers(o: HomeOverview, actions: HomeActions, vertical: Boolean = false) {
    val (inValue, inUnit) = if (o.trafficReady) formatRate(o.externalRx) else "—" to ""
    val (outValue, outUnit) = if (o.trafficReady) formatRate(o.externalTx) else "—" to ""
    val nodes: @Composable (Modifier) -> Unit = { m ->
        StatValue(stringResource(R.string.nodes_online), o.nodesOnline.toString(), m, "/ ${o.nodesTotal}") {
            actions.openNodes(offlineOnly = o.nodesOnline < o.nodesTotal)
        }
    }
    val containers: @Composable (Modifier) -> Unit = { m ->
        StatValue(stringResource(R.string.containers_running), o.containersRunning.toString(), m, "/ ${o.containersTotal}") {
            actions.openProjects(notRunningOnly = o.containersRunning < o.containersTotal)
        }
    }
    val rx: @Composable (Modifier) -> Unit = { m -> StatValue(stringResource(R.string.external_in), inValue, m, inUnit) }
    val tx: @Composable (Modifier) -> Unit = { m -> StatValue(stringResource(R.string.external_out), outValue, m, outUnit) }
    if (vertical) {
        Column(verticalArrangement = Arrangement.spacedBy(20.dp)) {
            listOf(nodes, containers, rx, tx).forEach { it(Modifier.fillMaxWidth()) }
        }
    } else {
        Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
            Row { nodes(Modifier.weight(1f)); containers(Modifier.weight(1f)) }
            Row { rx(Modifier.weight(1f)); tx(Modifier.weight(1f)) }
        }
    }
}

// ---- 实时流量 ----

/**
 * 实时流量的桑基图，和网页概览一样（docs/android.md#首页）：路径在手机上按网页的算法算，约 10 秒一份。
 * 选择项（全部、对外、节点之间）和标题在同一行、靠右；高度变化时下面的内容跟着移动。
 */
@Composable
private fun ColumnScope.FlowSection(o: HomeOverview, view: SankeyView, maxHeight: Dp? = null, fill: Boolean = false, setView: (SankeyView) -> Unit) {
    val views = SankeyView.entries
    SectionHeader(stringResource(R.string.flow_title)) {
        UnderlineTabs(
            listOf(stringResource(R.string.flow_view_all), stringResource(R.string.flow_view_ext), stringResource(R.string.flow_view_mesh)),
            selected = views.indexOf(view), onSelect = { setView(views[it]) },
        )
    }
    val panel = stringResource(R.string.flow_panel)
    val rows = buildList {
        if (view != SankeyView.Mesh) add(SankeyRow(SANKEY_EXTERNAL, stringResource(R.string.flow_external)))
        o.flowRows.forEach { add(SankeyRow(it.id, if (it.id == "__panel") panel else it.name, it.offline)) }
    }
    val flows = o.flows.map { SankeyFlow(it.from, it.to, it.rate, if (it.kind == "ext") FlowKind.External else FlowKind.Mesh, it.relay) }
    val texts = SankeyTexts(
        send = stringResource(R.string.flow_send), receive = stringResource(R.string.flow_receive),
        noSend = stringResource(R.string.flow_no_send), noReceive = stringResource(R.string.flow_no_receive),
        offline = stringResource(R.string.flow_offline), other = stringResource(R.string.flow_other), relayed = stringResource(R.string.flow_relayed),
    )
    val top = flows.maxByOrNull { it.rate }
    val nameOf = { id: String -> rows.firstOrNull { it.id == id }?.name ?: id }
    val description = top?.let { stringResource(R.string.flow_description, nameOf(it.from), nameOf(it.to), formatRateText(it.rate)) }
    val state = when {
        o.nodesTotal > 0 && o.nodesOnline == 0 -> 1
        !o.trafficReady -> 0
        flows.isEmpty() -> 2
        else -> 3
    }
    // 骨架、空状态和图之间淡入淡出，高度跟着过渡。[fill] 时（宽屏右边一栏）铺满标题以下的高度：量出剩下的高度交给桑基图。
    val limit = maxHeight
    BoxWithConstraints(Modifier.fillMaxWidth().then(if (fill) Modifier.weight(1f) else Modifier)) {
    // 铺满时用这一块实际剩下的高度；否则用传进来的上限（可以没有）。
    // 注意：这里的 maxHeight 会解析成上面函数的参数，要用 this.maxHeight 取这一块的高度。
    val room = if (fill && constraints.hasBoundedHeight) this.maxHeight else limit
    Column(Modifier.fillMaxWidth().autoHeight()) {
        AnimatedContent(
            targetState = state,
            transitionSpec = { fadeIn(tween(Motion.FADE_IN, easing = Motion.Enter)) togetherWith fadeOut(tween(100, easing = Motion.Exit)) },
            label = "flow",
        ) { st ->
            when (st) {
                0 -> SkeletonRows(3)
                1 -> FlowEmpty(stringResource(R.string.flow_all_offline))
                2 -> FlowEmpty(stringResource(R.string.flow_empty))
                else -> SankeyChart(rows, flows, view, texts, description = description, maxHeight = room, fillHeight = fill)
            }
        }
    }
    }
}

@Composable
private fun FlowEmpty(text: String) {
    Box(Modifier.fillMaxWidth().heightIn(min = Dimens.rowHeight), contentAlignment = Alignment.CenterStart) {
        Text(text, style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground)
    }
}

// ---- 流量趋势 ----

@Composable
private fun TrafficSection(state: TrafficState, range: TrafficRange, setRange: (TrafficRange) -> Unit, retry: () -> Unit) {
    val ranges = TrafficRange.entries
    SectionHeader(stringResource(R.string.traffic_title)) {
        UnderlineTabs(
            listOf(stringResource(R.string.range_hour), stringResource(R.string.range_day), stringResource(R.string.range_week)),
            selected = ranges.indexOf(range), onSelect = { setRange(ranges[it]) },
        )
    }
    val brand = NagiTheme.colors.brand
    val gray = NagiTheme.colors.mutedForeground
    ChartLegend(listOf(stringResource(R.string.traffic_external) to brand, stringResource(R.string.traffic_mesh) to gray))
    Spacer(Modifier.height(12.dp))
    AnimatedContent(
        targetState = state,
        contentKey = { it::class },
        transitionSpec = { fadeIn(tween(Motion.FADE_IN, easing = Motion.Enter)) togetherWith fadeOut(tween(100, easing = Motion.Exit)) },
        label = "traffic",
    ) { state -> TrafficBody(state, range, retry) }
}

@Composable
private fun TrafficBody(state: TrafficState, range: TrafficRange, retry: () -> Unit) {
    val brand = NagiTheme.colors.brand
    val gray = NagiTheme.colors.mutedForeground
    when (state) {
        TrafficState.Loading -> Skeleton(Modifier.fillMaxWidth().height(112.dp))
        is TrafficState.Failed -> InlineError(state.message, stringResource(R.string.retry), retry, height = 112.dp)
        is TrafficState.Ready -> {
            val h = state.history
            val ext = h.externalRx.zip(h.externalTx) { a, b -> (a + b).toFloat() }
            val mesh = h.mesh.map { it.toFloat() }
            val tipFmt = remember(range) { SimpleDateFormat(if (range == TrafficRange.WEEK) "M/d HH:mm" else "HH:mm", Locale.getDefault()) }
            LineChart(
                listOf(ChartLine(mesh, gray, stringResource(R.string.traffic_mesh)), ChartLine(ext, brand, stringResource(R.string.traffic_external))),
                Modifier.fillMaxWidth(), xLabels = timeLabels(h.times, range),
                tipTime = { i -> h.times.getOrNull(i)?.let { tipFmt.format(Date(it * 1000)) } ?: "" },
                tipFormat = { formatRateText(it.toDouble()) },
            )
        }
    }
}

/** 横轴的 5 个时间，最后一个写「现在」。 */
@Composable
private fun timeLabels(times: List<Long>, range: TrafficRange): List<String> {
    if (times.size < 2) return emptyList()
    val now = stringResource(R.string.now)
    val fmt = SimpleDateFormat(if (range == TrafficRange.WEEK) "M/d" else "HH:mm", Locale.getDefault())
    val first = times.first()
    val last = times.last()
    return (0 until 5).map { i ->
        if (i == 4) now else fmt.format(Date((first + (last - first) * i / 4) * 1000))
    }
}

// ---- 节点状态 ----

/**
 * 节点状态（和网页概览的节点表一样，精简成一行）：状态点、名称，右边是 CPU、内存、磁盘三个占用环（中间是图标，CPU 按核数折成 100%）。
 * 离线、待批准的在名称下面写出来，没有占用环。
 * 点一格去节点页。[columns] 为 2 时（宽屏左边一栏）两个一行。
 */
@Composable
private fun NodesSection(nodes: List<NodeStat>, actions: HomeActions, columns: Int = 1) {
    SectionHeader(stringResource(R.string.nodes_title))
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        nodes.chunked(columns).forEach { group ->
            Row(horizontalArrangement = Arrangement.spacedBy(24.dp)) {
                group.forEach { n -> NodeCell(n, Modifier.weight(1f)) { actions.openNode(n.id) } }
                // 最后一行只有一个时，空出另一半，宽度和上面的对齐。
                repeat(columns - group.size) { Spacer(Modifier.weight(1f)) }
            }
        }
    }
}

@Composable
private fun NodeCell(n: NodeStat, modifier: Modifier, onClick: () -> Unit) {
    val c = NagiTheme.colors
    val dot = when (n.status) {
        "online" -> c.success
        "pending" -> c.warning
        else -> c.mutedForeground
    }
    Row(modifier.rowClickable(onClick).padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
        StatusDot(dot)
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(n.name, style = NagiTheme.type.body, color = c.foreground, maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (n.status != "online") {
                Text(
                    stringResource(if (n.status == "pending") R.string.node_pending else R.string.node_offline),
                    style = NagiTheme.type.secondary, color = c.mutedForeground,
                )
            }
        }
        if (n.status == "online") {
            Spacer(Modifier.width(8.dp))
            val cpu = if (n.cpus > 0) n.cpu / (n.cpus * 100) else n.cpu / 100
            Ring("CPU", NagiIcons.Cpu, cpu)
            Spacer(Modifier.width(8.dp))
            Ring(stringResource(R.string.node_memory), NagiIcons.MemoryStick, n.memory / 100)
            Spacer(Modifier.width(8.dp))
            Ring(stringResource(R.string.node_disk), NagiIcons.HardDrive, if (n.diskTotal > 0) n.diskUsed / n.diskTotal else null)
        }
    }
}

/**
 * 一个占用环：26 的圆环，中间是这一项的图标（CPU、内存、磁盘），不写数字。到 80% 变警告色、90% 变红；
 * 不知道总量时（[fraction] 为 null）只有灰色的底环。弧的长度变化有过渡。读屏读「CPU 16%」。
 */
@Composable
private fun Ring(label: String, icon: ImageVector, fraction: Double?) {
    val c = NagiTheme.colors
    val f = fraction?.takeIf { it.isFinite() }?.coerceIn(0.0, 1.0)
    val color = when {
        f == null -> c.mutedForeground
        f >= 0.9 -> c.destructive
        f >= 0.8 -> c.warning
        else -> c.brand
    }
    val sweep by animateFloatAsState(((f ?: 0.0) * 360).toFloat(), barSpec(), label = "ring")
    Box(
        Modifier.size(26.dp).semantics(mergeDescendants = true) { contentDescription = "$label ${f?.let { "%.0f%%".format(it * 100) } ?: "—"}" },
        contentAlignment = Alignment.Center,
    ) {
        val track = c.muted
        Canvas(Modifier.size(26.dp)) {
            val stroke = 2.5.dp.toPx()
            val inset = stroke / 2
            val arc = Size(size.width - stroke, size.height - stroke)
            drawArc(track, 0f, 360f, false, Offset(inset, inset), arc, style = Stroke(stroke))
            if (sweep > 0f) drawArc(color, -90f, sweep, false, Offset(inset, inset), arc, style = Stroke(stroke, cap = StrokeCap.Round))
        }
        Icon(icon, contentDescription = null, tint = c.mutedForeground, modifier = Modifier.size(12.dp))
    }
}


// ---- 流量最大的映射 ----

@Composable
/** 流量最大的映射：最近 24 小时流量合计最多的 3 条（和网页概览一样），一行是地址和合计，下面一条按最大的一条算长度的条。 */
private fun TopRoutes(routes: List<RouteRate>) {
    SectionHeader(stringResource(R.string.top_routes)) {
        Text(stringResource(R.string.range_day), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
    }
    val max = routes.maxOf { it.rx + it.tx }.coerceAtLeast(1.0)
    // 节点看不到（访问范围外）时面板不给名称和地址：写「其他节点」。
    val other = stringResource(R.string.other_node)
    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        routes.forEach { r ->
            UsageRow(r.address.ifEmpty { if (r.port > 0) "$other:${r.port}" else other }, formatBytesText(r.rx + r.tx), ((r.rx + r.tx) / max).toFloat(), mono = true)
        }
    }
}

// ---- 状态 ----

@Composable
private fun LoadingBody(modifier: Modifier = Modifier) {
    Column(modifier.padding(horizontal = Dimens.pageHorizontal)) {
        Spacer(Modifier.height(Dimens.topBarToContent))
        Skeleton(Modifier.width(96.dp).height(16.dp))
        Spacer(Modifier.height(16.dp))
        SkeletonRows(3)
        SectionDivider()
        SkeletonRows(2)
        SectionDivider()
        Skeleton(Modifier.fillMaxWidth().height(112.dp))
    }
}

/** 连不上面板（s8 第 2 张）：有数据时写是哪个时间的，正在重试。 */
@Composable
private fun Unreachable(updatedAt: Long?, reason: String, retry: () -> Unit) {
    val time = if (updatedAt != null) {
        stringResource(R.string.unreachable_cached, SimpleDateFormat("HH:mm", Locale.getDefault()).format(Date(updatedAt)))
    } else {
        stringResource(R.string.unreachable_retrying)
    }
    Banner(stringResource(R.string.unreachable_title), lines = listOf(time, reason), action = stringResource(R.string.retry), onAction = retry)
}

