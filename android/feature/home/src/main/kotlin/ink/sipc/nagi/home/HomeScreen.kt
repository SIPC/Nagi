package ink.sipc.nagi.home

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withStyle
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
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SectionDivider
import ink.sipc.nagi.ui.SectionHeader
import ink.sipc.nagi.ui.Skeleton
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.StatusDot
import ink.sipc.nagi.ui.UnderlineTabs
import ink.sipc.nagi.ui.UsageBar
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
 * 顶部栏和胶囊导航由外层（App）放。[contentPadding] 的底部留出胶囊的 96。
 */
@Composable
fun HomeContent(vm: HomeViewModel, actions: HomeActions, contentPadding: PaddingValues, showMessage: (String) -> Unit) {
    val state by vm.overview.collectAsStateWithLifecycle()
    val traffic by vm.traffic.collectAsStateWithLifecycle()
    val range by vm.range.collectAsStateWithLifecycle()
    val approving by vm.approving.collectAsStateWithLifecycle()
    LaunchedEffect(vm) { vm.messages.collect(showMessage) }
    val approved = stringResource(R.string.approved)

    LazyColumn(Modifier.fillMaxSize(), contentPadding = contentPadding) {
        when (val s = state) {
            OverviewState.Loading, OverviewState.Locked, OverviewState.Removed -> item { LoadingBody() }
            is OverviewState.Failed -> item {
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
                if (!s.live) item { Unreachable(s.updatedAt, s.error.orEmpty(), vm::refresh) }
                item {
                    Column(Modifier.padding(horizontal = Dimens.pageHorizontal)) {
                        Spacer(Modifier.height(Dimens.topBarToContent))
                        AttentionSection(s.overview, approving, actions) { item ->
                            vm.approve(item.nodeId, item.title) { approved }
                        }
                        SectionDivider()
                        KeyNumbers(s.overview, actions)
                        SectionDivider()
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
    val items = o.attention.itemsList
    var expanded by remember { mutableStateOf(false) }
    SectionHeader(stringResource(R.string.attention_title)) {
        if (items.isNotEmpty()) Text(items.size.toString(), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
    }
    if (items.isEmpty()) {
        Row(Modifier.heightIn(min = Dimens.attentionRow), verticalAlignment = Alignment.CenterVertically) {
            StatusDot(NagiTheme.colors.success)
            Spacer(Modifier.width(12.dp))
            Text(stringResource(R.string.attention_none), style = NagiTheme.type.body, color = NagiTheme.colors.foreground)
        }
        return
    }
    val shown = if (expanded || items.size <= ATTENTION_LIMIT) items else items.take(ATTENTION_LIMIT - 1)
    shown.forEach { AttentionRow(it, it.nodeId in approving, actions, approve) }
    if (shown.size < items.size) {
        Row(
            Modifier.fillMaxWidth().heightIn(min = Dimens.attentionRow).clickable { expanded = true },
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Spacer(Modifier.width(18.dp))
            Text(
                stringResource(R.string.attention_more, items.size - shown.size), style = NagiTheme.type.body,
                color = NagiTheme.colors.mutedForeground, modifier = Modifier.weight(1f),
            )
        }
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
    val detail = attentionDetail(item)
    Row(
        Modifier.fillMaxWidth()
            .heightIn(min = if (detail.isEmpty()) Dimens.attentionRow else Dimens.attentionRowTwoLine)
            .clickable { actions.openAttention(item) }
            .padding(vertical = 6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        // 点和第一行对齐。
        Box(Modifier.align(if (detail.isEmpty()) Alignment.CenterVertically else Alignment.Top).padding(top = if (detail.isEmpty()) 0.dp else 8.dp)) {
            StatusDot(dot)
        }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(item.title, style = NagiTheme.type.body, color = colors.foreground, maxLines = 2, overflow = TextOverflow.Ellipsis)
            if (detail.isNotEmpty()) {
                Text(detail, style = NagiTheme.type.secondary, color = colors.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis)
            }
        }
        Spacer(Modifier.width(12.dp))
        when (item.action) {
            "approve" -> NagiButton(
                stringResource(R.string.action_approve), { approve(item) }, variant = ButtonVariant.Outline, size = ButtonSize.Small, loading = busy,
            )
            "update" -> NagiButton(stringResource(R.string.action_update), { actions.update(item) }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
            else -> Icon(NagiIcons.ChevronRight, contentDescription = null, tint = colors.mutedForeground, modifier = Modifier.size(16.dp))
        }
    }
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
            Number(stringResource(R.string.nodes_online), o.nodesOnline.toString(), "/ ${o.nodesTotal}", Modifier.weight(1f)) {
                actions.openNodes(offlineOnly = o.nodesOnline < o.nodesTotal)
            }
            Number(stringResource(R.string.containers_running), o.containersRunning.toString(), "/ ${o.containersTotal}", Modifier.weight(1f)) {
                actions.openProjects(notRunningOnly = o.containersRunning < o.containersTotal)
            }
        }
        Row {
            val (inValue, inUnit) = if (o.trafficReady) formatRate(o.externalRx) else "—" to ""
            val (outValue, outUnit) = if (o.trafficReady) formatRate(o.externalTx) else "—" to ""
            Number(stringResource(R.string.external_in), inValue, inUnit, Modifier.weight(1f), null)
            Number(stringResource(R.string.external_out), outValue, outUnit, Modifier.weight(1f), null)
        }
    }
}

@Composable
private fun Number(label: String, value: String, suffix: String, modifier: Modifier, onClick: (() -> Unit)?) {
    Column(modifier.then(if (onClick != null) Modifier.clickable(onClick = onClick) else Modifier)) {
        Text(label, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
        Spacer(Modifier.height(4.dp))
        Text(
            buildAnnotatedString {
                append(value)
                if (suffix.isNotEmpty()) {
                    withStyle(SpanStyle(fontSize = NagiTheme.type.secondary.fontSize, color = NagiTheme.colors.mutedForeground)) { append(" $suffix") }
                }
            },
            style = NagiTheme.type.keyNumber, color = NagiTheme.colors.foreground,
        )
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
    when (state) {
        TrafficState.Loading -> Skeleton(Modifier.fillMaxWidth().height(112.dp))
        is TrafficState.Failed -> Row(Modifier.fillMaxWidth().height(112.dp), verticalAlignment = Alignment.CenterVertically) {
            Text(state.message, style = NagiTheme.type.body, color = NagiTheme.colors.destructive, modifier = Modifier.weight(1f))
            NagiButton(stringResource(R.string.retry), retry, variant = ButtonVariant.Ghost, size = ButtonSize.Small)
        }
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
            Column {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        r.address, style = NagiTheme.type.mono, color = NagiTheme.colors.foreground, maxLines = 1,
                        overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f),
                    )
                    Spacer(Modifier.width(12.dp))
                    val (v, u) = formatRate(r.rx + r.tx)
                    Text("$v $u", style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
                }
                Spacer(Modifier.height(6.dp))
                UsageBar(((r.rx + r.tx) / max).toFloat(), NagiTheme.colors.brand, Modifier.fillMaxWidth())
            }
        }
    }
}

// ---- 状态 ----

@Composable
private fun LoadingBody() {
    Column(Modifier.padding(horizontal = Dimens.pageHorizontal)) {
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
    val colors = NagiTheme.colors
    Row(Modifier.fillMaxWidth().background(colors.muted).padding(horizontal = Dimens.pageHorizontal, vertical = 12.dp)) {
        Box(Modifier.padding(top = 7.dp)) { StatusDot(colors.warning) }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(stringResource(R.string.unreachable_title), style = NagiTheme.type.bodyMedium, color = colors.foreground)
            val text = if (updatedAt != null) {
                stringResource(R.string.unreachable_cached, SimpleDateFormat("HH:mm", Locale.getDefault()).format(Date(updatedAt)))
            } else {
                stringResource(R.string.unreachable_retrying)
            }
            Text(text, style = NagiTheme.type.secondary, color = colors.mutedForegroundStrong)
            if (reason.isNotEmpty()) Text(reason, style = NagiTheme.type.secondary, color = colors.mutedForegroundStrong, maxLines = 2)
            Spacer(Modifier.height(4.dp))
            Text(
                stringResource(R.string.retry), style = NagiTheme.type.bodyMedium, color = colors.foreground,
                modifier = Modifier.clickable(onClick = retry).padding(vertical = 8.dp),
            )
        }
    }
}

