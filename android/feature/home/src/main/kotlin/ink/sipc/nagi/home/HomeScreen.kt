package ink.sipc.nagi.home

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
import ink.sipc.nagi.proto.v1.AttentionItem
import ink.sipc.nagi.proto.v1.Overview
import ink.sipc.nagi.proto.v1.RouteTraffic
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
    fun openAttention(item: AttentionItem)
    fun openNodes(offlineOnly: Boolean)
    fun openProjects(notRunningOnly: Boolean)
    fun update(item: AttentionItem)
}

/**
 * 首页（s1 第 1 张、s8 状态）：需要处理、关键数字、流量趋势、流量最大的映射。数据来自 WatchOverview 订阅流。
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
                        Collapse(s.overview.attention.itemsCount > 0) {
                            AttentionSection(s.overview, approving, actions) { item ->
                                vm.approve(item.nodeId, item.title) { approved }
                            }
                            SectionDivider()
                        }
                        KeyNumbers(s.overview, actions)
                        SectionDivider()
                        // 旧版本的面板不发桑基图的数据（没有行），这时不显示这一块。
                        if (s.overview.flowRowsCount > 0) {
                            FlowSection(s.overview, flowView) { flowView = it }
                            SectionDivider()
                        }
                        TrafficSection(traffic, range, vm::setRange, vm::loadTraffic)
                        if (s.overview.topRoutesCount > 0) {
                            SectionDivider()
                            TopRoutes(s.overview.topRoutesList)
                        }
                    }
                }
            }
        }
    }
}

// ---- 需要处理 ----

@Composable
private fun AttentionSection(o: Overview, approving: Set<String>, actions: HomeActions, approve: (AttentionItem) -> Unit) {
    // 收起的过程中还显示最后的那些事项，不闪成空的。
    val last = remember { arrayOf(o.attention.itemsList) }
    if (o.attention.itemsCount > 0) last[0] = o.attention.itemsList
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
    items: List<AttentionItem>,
    expanded: Boolean,
    expand: () -> Unit,
    approving: Set<String>,
    actions: HomeActions,
    approve: (AttentionItem) -> Unit,
) {
    val shown = if (expanded || items.size <= ATTENTION_LIMIT) items else items.take(ATTENTION_LIMIT - 1)
    shown.forEach { AttentionRow(it, it.nodeId in approving, actions, approve) }
    if (shown.size < items.size) {
        MoreRow(stringResource(R.string.attention_more, items.size - shown.size), expand, density = RowDensity.Compact)
    }
}

@Composable
private fun AttentionRow(item: AttentionItem, busy: Boolean, actions: HomeActions, approve: (AttentionItem) -> Unit) {
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
        item.title, detail = attentionDetail(item), dot = dot, density = RowDensity.Compact,
        onClick = { actions.openAttention(item) }, trailing = trailing,
    )
}

/** 第二行：对象和时长，例如「已离线 4 分钟」「node1 · 12 分钟」。 */
@Composable
private fun attentionDetail(item: AttentionItem): String {
    val since = if (item.hasSince() && item.since.seconds > 0) durationText(item.since.seconds) else ""
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

@Composable
private fun KeyNumbers(o: Overview, actions: HomeActions) {
    Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
        Row {
            StatValue(stringResource(R.string.nodes_online), o.nodesOnline.toString(), Modifier.weight(1f), "/ ${o.nodesTotal}") {
                actions.openNodes(offlineOnly = o.nodesOnline < o.nodesTotal)
            }
            StatValue(stringResource(R.string.containers_running), o.containersRunning.toString(), Modifier.weight(1f), "/ ${o.containersTotal}") {
                actions.openProjects(notRunningOnly = o.containersRunning < o.containersTotal)
            }
        }
        Row {
            val (inValue, inUnit) = if (o.trafficReady) formatRate(o.externalRx) else "—" to ""
            val (outValue, outUnit) = if (o.trafficReady) formatRate(o.externalTx) else "—" to ""
            StatValue(stringResource(R.string.external_in), inValue, Modifier.weight(1f), inUnit)
            StatValue(stringResource(R.string.external_out), outValue, Modifier.weight(1f), outUnit)
        }
    }
}

// ---- 实时流量 ----

/**
 * 实时流量的桑基图，和网页概览一样（docs/android.md#首页）：路径由面板算好放在订阅流里，约 10 秒一份。
 * 选择项（全部、对外、节点之间）和标题在同一行、靠右；高度变化时下面的内容跟着移动。
 */
@Composable
private fun FlowSection(o: Overview, view: SankeyView, setView: (SankeyView) -> Unit) {
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
        o.flowRowsList.forEach { add(SankeyRow(it.id, if (it.id == "__panel") panel else it.name, it.offline)) }
    }
    val flows = o.flowsList.map { SankeyFlow(it.from, it.to, it.rate, if (it.kind == "ext") FlowKind.External else FlowKind.Mesh, it.relay) }
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
    // 骨架、空状态和图之间淡入淡出，高度跟着过渡。
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
                else -> SankeyChart(rows, flows, view, texts, description = description)
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
            val ext = h.externalRxList.zip(h.externalTxList) { a, b -> (a + b).toFloat() }
            val mesh = h.meshList.map { it.toFloat() }
            LineChart(listOf(ChartLine(mesh, gray), ChartLine(ext, brand)), Modifier.fillMaxWidth(), xLabels = timeLabels(h.timesList, range))
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

// ---- 流量最大的映射 ----

@Composable
private fun TopRoutes(routes: List<RouteTraffic>) {
    SectionHeader(stringResource(R.string.top_routes))
    val max = routes.maxOf { it.rx + it.tx }.coerceAtLeast(1.0)
    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        routes.forEach { r ->
            val (v, u) = formatRate(r.rx + r.tx)
            UsageRow(r.address, "$v $u", ((r.rx + r.tx) / max).toFloat(), mono = true)
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

