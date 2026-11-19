package ink.sipc.nagi.nodes

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import com.google.protobuf.ByteString
import ink.sipc.nagi.data.Access
import ink.sipc.nagi.data.LatencyHistory
import ink.sipc.nagi.data.MAP_PANEL
import ink.sipc.nagi.data.MapRepo
import ink.sipc.nagi.data.MeshKind
import ink.sipc.nagi.data.MeshLink
import ink.sipc.nagi.data.MeshSnapshot
import ink.sipc.nagi.data.NodeList
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.data.Polled
import ink.sipc.nagi.data.RelayReason
import ink.sipc.nagi.data.TrafficRange
import ink.sipc.nagi.data.buildMeshLinks
import ink.sipc.nagi.data.formatMs
import ink.sipc.nagi.data.keepLast
import ink.sipc.nagi.data.live
import ink.sipc.nagi.data.mapCurveAt
import ink.sipc.nagi.data.mapLayout
import ink.sipc.nagi.data.meshCells
import ink.sipc.nagi.data.meshControl
import ink.sipc.nagi.data.meshLabelAt
import ink.sipc.nagi.data.meshListOrder
import ink.sipc.nagi.data.meshRank
import ink.sipc.nagi.data.observedRtt
import ink.sipc.nagi.data.poll
import ink.sipc.nagi.data.relayReason
import ink.sipc.nagi.data.relayVia
import ink.sipc.nagi.data.usageInfo
import ink.sipc.nagi.proto.v1.MeshStatus
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.ui.ChartLegend
import ink.sipc.nagi.ui.ChartLine
import ink.sipc.nagi.ui.Collapse
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.EmptyState
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.LineChart
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.NagiTopBar
import ink.sipc.nagi.ui.Roll
import ink.sipc.nagi.ui.SectionDivider
import ink.sipc.nagi.ui.SectionHeader
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.StatusDot
import ink.sipc.nagi.ui.UnderlineTabs
import ink.sipc.nagi.ui.autoHeight
import ink.sipc.nagi.ui.mouseHover
import ink.sipc.nagi.ui.rowClickable
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.mapNotNull
import kotlinx.coroutines.flow.stateIn
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

// 节点网络（docs/android.md「节点网络」，同网页 nodes.tsx 的网络视图和 mesh.tsx）：手机上不画拓扑图，最上面一行写节点数、
// 连接数和未直连的条数，下面先列未直连的连接（不可达、中转，写原因），再列直连，按延迟从低到高；点一条看两个方向的延迟历史。
// 可用宽度达到 MapMinWidth 时画拓扑图，下面是未直连的连接和节点间延迟，和网页一样。

/** 在线节点和面板的节点网络状态：每 10 秒查一次（同网页），节点增减时马上重查，换的时候留着上一份。 */
@OptIn(ExperimentalCoroutinesApi::class)
class MeshViewModel(val repo: MapRepo, list: StateFlow<Polled<NodeList>>) : ViewModel() {
    private val refreshes = Channel<Unit>(Channel.CONFLATED)

    val mesh: StateFlow<Polled<MeshSnapshot>> = list
        .mapNotNull { (it as? Polled.Ready)?.value }
        .map { l -> l.nodes.filter { it.live }.map { it.id } to l.access.isAdmin }
        .distinctUntilChanged()
        .flatMapLatest { (ids, admin) -> poll(10_000, refreshes) { repo.mesh(ids, admin) } }
        .keepLast()
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), Polled.Loading)

    fun retry() {
        refreshes.trySend(Unit)
    }
}

/**
 * 节点网络整页（要「查看节点」）。[onBack] 为 null 时（宽屏右边一栏的第一页）顶部栏没有返回。
 * 点节点（拓扑图上、列表里的名称）用 [openNode] 打开节点页。
 */
@Composable
fun MeshScreen(list: NodesViewModel, session: PanelSession, onBack: (() -> Unit)?, openNode: (String) -> Unit, mapMinWidth: Dp = MapMinWidth) {
    val vm: MeshViewModel = viewModel(key = "mesh-" + session.panel.id, factory = Factory { MeshViewModel(MapRepo(session), list.list) })
    val state by list.list.collectAsStateWithLifecycle()
    val title = stringResource(R.string.mesh_title)
    Column(Modifier.fillMaxSize().navigationBarsPadding()) {
        if (onBack != null) NagiBackTopBar(title, onBack = onBack) else NagiTopBar(title)
        when (val s = state) {
            Polled.Loading -> SkeletonRows(4, Modifier.padding(horizontal = Dimens.pageHorizontal))
            is Polled.Failed -> InlineError(s.message, stringResource(R.string.retry), list::refresh, Modifier.padding(horizontal = Dimens.pageHorizontal))
            is Polled.Ready -> when {
                !s.value.access.can(Access.NODE_VIEW) -> EmptyState(stringResource(R.string.nodes_denied))
                s.value.nodes.isEmpty() -> EmptyState(stringResource(R.string.nodes_empty))
                else -> MeshBody(vm, s.value.nodes, openNode, mapMinWidth)
            }
        }
    }
}

@Composable
private fun MeshBody(vm: MeshViewModel, nodes: List<Node>, openNode: (String) -> Unit, mapMinWidth: Dp) {
    val state by vm.mesh.collectAsStateWithLifecycle()
    BoxWithConstraints(Modifier.fillMaxSize()) {
        val wide = maxWidth >= mapMinWidth
        val avail = maxWidth - Dimens.pageHorizontal * 2
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = Dimens.pageHorizontal).padding(bottom = 24.dp)) {
            Spacer(Modifier.height(Dimens.topBarToContent))
            when (val s = state) {
                Polled.Loading -> SkeletonRows(4)
                is Polled.Failed -> InlineError(s.message, stringResource(R.string.retry), vm::retry)
                is Polled.Ready -> {
                    val snap = s.value
                    val live = nodes.filter { it.live }
                    val links = buildMeshLinks(nodes, snap.statuses, snap.panel, (live.map { it.id } + MAP_PANEL).toSet())
                    val names = Names(nodes, stringResource(R.string.map_panel))
                    // 查不到的一端只能按默认样式画，所以写出来；有旧数据时偶尔失败一次不提示。
                    val failed = @Composable { if (snap.failed > 0) InlineError(snap.error.orEmpty(), stringResource(R.string.retry), vm::retry) }
                    if (wide) WideMesh(vm, nodes, live, links, snap, names, avail, openNode, failed)
                    else PhoneMesh(vm, live, links, snap, names, failed)
                }
            }
        }
    }
}

/** 节点 ID 换成名称，面板写「面板」。 */
private class Names(nodes: List<Node>, private val panel: String) {
    private val map = nodes.associate { it.id to it.name }
    operator fun get(id: String): String = if (id == MAP_PANEL) panel else map[id] ?: id
}

// ---- 手机：列表 ----

@Composable
private fun PhoneMesh(vm: MeshViewModel, live: List<Node>, links: List<MeshLink>, snap: MeshSnapshot, names: Names, failed: @Composable () -> Unit) {
    val c = NagiTheme.colors
    val shown = meshListOrder(links)
    val issues = shown.filter { it.kind != MeshKind.DIRECT }
    val direct = shown.filter { it.kind == MeshKind.DIRECT }
    var open by rememberSaveable { mutableStateOf<String?>(null) }
    val summary = buildAnnotatedString {
        append(stringResource(R.string.mesh_count_nodes, live.size))
        append(" · ")
        append(stringResource(R.string.mesh_count_links, shown.size))
        if (issues.isNotEmpty()) {
            append(" · ")
            withStyle(SpanStyle(color = c.warningText)) { append(stringResource(R.string.mesh_count_issues, issues.size)) }
        }
    }
    Spacer(Modifier.height(8.dp))
    Text(summary, style = NagiTheme.type.secondary, color = c.mutedForeground)
    Spacer(Modifier.height(8.dp))
    failed()
    Column(Modifier.fillMaxWidth().autoHeight()) {
        if (shown.isEmpty()) {
            Text(stringResource(R.string.mesh_no_links), style = NagiTheme.type.body, color = c.mutedForeground, modifier = Modifier.padding(vertical = 14.dp))
        }
        if (issues.isNotEmpty()) {
            SectionHeader(stringResource(R.string.mesh_issues)) { Roll("${issues.size}", NagiTheme.type.secondary, c.mutedForeground) }
            for (l in issues) LinkWithHistory(vm, l, snap, names, open == l.key) { open = if (open == l.key) null else l.key }
        }
        if (direct.isNotEmpty()) {
            if (issues.isNotEmpty()) SectionDivider()
            SectionHeader(stringResource(R.string.mesh_direct_links)) { Roll("${direct.size}", NagiTheme.type.secondary, c.mutedForeground) }
            for (l in direct) LinkWithHistory(vm, l, snap, names, open == l.key) { open = if (open == l.key) null else l.key }
        }
    }
}

/** 一条连接；能查延迟历史时点了在下面展开两个方向的延迟，再点收起。 */
@Composable
private fun LinkWithHistory(vm: MeshViewModel, l: MeshLink, snap: MeshSnapshot, names: Names, open: Boolean, onToggle: () -> Unit) {
    val ends = remember(l, snap) { historyEnds(l, snap) }
    LinkRow(l, snap.statuses, names, selected = open, onClick = if (ends != null) onToggle else null)
    if (ends != null) {
        Collapse(open) {
            Column(Modifier.padding(top = 4.dp, bottom = 12.dp)) { LatencyChart(vm.repo, ends, names) }
        }
    }
}

/**
 * 连接的一行：状态点、「A ↔ B」，第二行是路径或没有直连的原因（原因较长，最多三行）；右边是延迟，没有延迟时写路径。
 */
@Composable
private fun LinkRow(
    l: MeshLink,
    statuses: Map<String, MeshStatus>,
    names: Names,
    selected: Boolean = false,
    onClick: (() -> Unit)? = null,
    modifier: Modifier = Modifier,
) {
    val c = NagiTheme.colors
    val rtt = l.observedRtt()
    val reason = relayReason(l, statuses)?.let { reasonText(it, names) }
    val path = edgePathLabel(l, names)
    val detail = if (l.kind == MeshKind.DIRECT) path else reason ?: path
    val trailing = if (rtt > 0) formatMs(rtt) else if (l.kind == MeshKind.DIRECT) "—" else stringResource(if (l.kind == MeshKind.NONE) R.string.map_none else R.string.map_relay)
    val dot = when (l.kind) {
        MeshKind.NONE -> c.destructive
        MeshKind.RELAY -> c.warning
        MeshKind.DIRECT -> c.success
    }
    Row(
        modifier.fillMaxWidth()
            .then(if (onClick != null) Modifier.rowClickable(onClick, highlight = if (selected) c.muted else null) else Modifier)
            .heightIn(min = Dimens.rowHeightTwoLine)
            .padding(vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.align(Alignment.Top).padding(top = 7.dp)) { StatusDot(dot) }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(stringResource(R.string.mesh_pair, names[l.a], names[l.b]), style = NagiTheme.type.body, color = c.foreground, maxLines = 1, overflow = TextOverflow.Ellipsis)
            Text(detail, style = NagiTheme.type.secondary, color = c.mutedForeground, maxLines = 3, overflow = TextOverflow.Ellipsis)
        }
        Spacer(Modifier.width(12.dp))
        val color = when (l.kind) {
            MeshKind.NONE -> c.destructive
            MeshKind.RELAY -> c.warningText
            MeshKind.DIRECT -> if (rtt > 0) c.foreground else c.mutedForeground
        }
        Roll(trailing, NagiTheme.type.secondary, color)
    }
}

/** 没有直连的原因。 */
@Composable
private fun reasonText(r: RelayReason, names: Names): String = when (r) {
    is RelayReason.Symmetric -> stringResource(R.string.mesh_reason_symmetric, names[r.a], names[r.b], r.symmetric.joinToString(stringResource(R.string.list_separator)) { names[it] })
    is RelayReason.NoAddress -> stringResource(R.string.mesh_reason_no_address, names[r.node])
    is RelayReason.OneWay -> stringResource(R.string.mesh_reason_one_way, names[r.blocked], names[r.from], r.port)
    is RelayReason.NoReply -> stringResource(R.string.mesh_reason_no_reply, names[r.a], r.portA, names[r.b], r.portB)
}

/** 连接的路径，经节点中转时写出经过的节点。 */
@Composable
private fun edgePathLabel(l: MeshLink, names: Names): String {
    val via = l.relayVia()
    return if (l.path == "node-relay" && via.isNotEmpty()) stringResource(R.string.mesh_via_node, via) else pathLabel(l.path)
}

/** 线上的标签：直连写延迟，其余写路径。 */
@Composable
private fun meshLabel(l: MeshLink, names: Names): String =
    if (l.kind == MeshKind.DIRECT) (if (l.rtt > 0) formatMs(l.rtt) else if (l.path.isNotEmpty()) pathLabel(l.path) else "") else edgePathLabel(l, names)

// ---- 延迟历史 ----

/** 查延迟历史要的两端：节点 ID（面板为 null，面板不存监控）和 mesh 公钥。 */
private data class HistoryEnds(val a: String, val nodeA: String?, val keyA: ByteString, val b: String, val nodeB: String?, val keyB: ByteString) {
    val key: String get() = "$a|$b"
}

/** 两端的公钥：从各自的状态取，没有时从对方看到的成员里取。一端也查不了时为 null。 */
private fun historyEnds(l: MeshLink, snap: MeshSnapshot): HistoryEnds? {
    fun keyOf(id: String, other: String): ByteString =
        (if (id == MAP_PANEL) snap.panel?.publicKey else snap.statuses[id]?.publicKey)?.takeIf { !it.isEmpty }
            ?: l.views[other]?.publicKey ?: ByteString.EMPTY
    val ka = keyOf(l.a, l.b)
    val kb = keyOf(l.b, l.a)
    val na = l.a.takeIf { it != MAP_PANEL }
    val nb = l.b.takeIf { it != MAP_PANEL }
    val canAB = na != null && !kb.isEmpty
    val canBA = nb != null && !ka.isEmpty
    if (!canAB && !canBA) return null
    return HistoryEnds(l.a, na, ka, l.b, nb, kb)
}

/**
 * 两个方向的延迟历史：时间范围（1 小时、24 小时、7 天）、图例和折线图，每 10 秒查一次（同网页）。
 * 换时间范围时先留着上一份，新的到了再换。
 */
@Composable
private fun LatencyChart(repo: MapRepo, e: HistoryEnds, names: Names) {
    val c = NagiTheme.colors
    var range by rememberSaveable { mutableStateOf(TrafficRange.HOUR) }
    val refresh = remember(e, range) { Channel<Unit>(Channel.CONFLATED) }
    val flow = remember(e, range) { poll(10_000, refresh) { repo.latency(e.nodeA, e.keyA, e.nodeB, e.keyB, range) } }
    val state by flow.collectAsStateWithLifecycle(Polled.Loading)
    val last = remember(e) { mutableStateOf<LatencyHistory?>(null) }
    (state as? Polled.Ready)?.let { last.value = it.value }
    val ranges = TrafficRange.entries
    val ab = stringResource(R.string.mesh_direction, names[e.a], names[e.b])
    val ba = stringResource(R.string.mesh_direction, names[e.b], names[e.a])
    UnderlineTabs(
        listOf(stringResource(R.string.range_hour), stringResource(R.string.range_day), stringResource(R.string.range_week)),
        selected = ranges.indexOf(range), onSelect = { range = ranges[it] }, spacing = 20.dp,
    )
    Spacer(Modifier.height(8.dp))
    val h = last.value
    val failed = state as? Polled.Failed
    when {
        h != null -> {
            // 两个方向用深浅区分，都是正常数据（同网页）。
            val lines = listOfNotNull(
                ChartLine(h.ab.map { it.toFloat() }, c.foreground, ab).takeIf { e.nodeA != null },
                ChartLine(h.ba.map { it.toFloat() }, c.mutedForeground, ba).takeIf { e.nodeB != null },
            )
            ChartLegend(lines.map { it.label to it.color })
            Spacer(Modifier.height(8.dp))
            if (h.times.size < 2) {
                Box(Modifier.fillMaxWidth().height(112.dp), contentAlignment = Alignment.Center) {
                    Text(stringResource(R.string.mesh_history_empty), style = NagiTheme.type.secondary, color = c.mutedForeground)
                }
            } else {
                val tip = remember(range) { SimpleDateFormat(if (range == TrafficRange.WEEK) "M/d HH:mm" else "HH:mm", Locale.getDefault()) }
                LineChart(
                    lines, Modifier.fillMaxWidth(), xLabels = timeLabels(h.times, range),
                    tipTime = { i -> h.times.getOrNull(i)?.let { tip.format(Date(it * 1000)) } ?: "" },
                    tipFormat = { v -> if (v.isFinite()) formatMs(v.toDouble()) else "—" },
                )
            }
        }
        failed != null -> InlineError(failed.message, stringResource(R.string.retry), { refresh.trySend(Unit) }, height = 112.dp)
        else -> Spacer(Modifier.height(140.dp))
    }
}

/** 横轴的 5 个时间，最后一个写「现在」（同节点页的监控）。 */
@Composable
private fun timeLabels(times: List<Long>, range: TrafficRange): List<String> {
    if (times.size < 2) return emptyList()
    val now = stringResource(R.string.now)
    val fmt = SimpleDateFormat(if (range == TrafficRange.WEEK) "M/d" else "HH:mm", Locale.getDefault())
    val first = times.first()
    val last = times.last()
    return (0 until 5).map { i -> if (i == 4) now else fmt.format(Date((first + (last - first) * i / 4) * 1000)) }
}

// ---- 宽屏：拓扑图 ----

@Composable
private fun WideMesh(
    vm: MeshViewModel,
    nodes: List<Node>,
    live: List<Node>,
    links: List<MeshLink>,
    snap: MeshSnapshot,
    names: Names,
    avail: Dp,
    openNode: (String) -> Unit,
    failed: @Composable () -> Unit,
) {
    val c = NagiTheme.colors
    var hover by remember { mutableStateOf<String?>(null) }
    var pinned by rememberSaveable { mutableStateOf<String?>(null) }
    // 悬停（鼠标）或点选（手指）的节点：突出它的连接，其他的淡下去。手指点一下先突出，再点一下打开节点页。
    val focus = hover ?: pinned
    val layout = remember(nodes.map { it.id }, avail) { mapLayout(nodes.map { it.id }, avail.value) }
    val panelAt = layout.pos.getValue(MAP_PANEL)
    val neighbors = links.filter { it.touches(focus) }.flatMap { listOf(it.a, it.b) }.toSet()
    fun dim(id: String) = focus != null && id !in neighbors && id != focus
    val lines = links.map { l ->
        val a = layout.pos.getValue(l.a)
        val b = layout.pos.getValue(l.b)
        val on = l.touches(focus)
        MapLine(
            l.key, a, meshControl(l, a, b, panelAt), b,
            color = when (l.kind) {
                MeshKind.RELAY -> c.warning
                MeshKind.NONE -> c.destructive
                // 直连的灰线和图例同一个透明度；突出时加深。
                MeshKind.DIRECT -> if (on) c.foreground.copy(alpha = 0.7f) else c.mutedForeground.copy(alpha = 0.6f)
            },
            alpha = if (focus != null && !on) 0.15f else 1f,
            dash = when (l.kind) {
                MeshKind.RELAY -> DashRelay
                MeshKind.NONE -> DashNone
                MeshKind.DIRECT -> null
            },
        )
    }
    val tags = links.filter { it.touches(focus) }.mapNotNull { l ->
        val text = meshLabel(l, names).takeIf { it.isNotEmpty() } ?: return@mapNotNull null
        val a = layout.pos.getValue(l.a)
        val b = layout.pos.getValue(l.b)
        MapTag(
            l.key, mapCurveAt(a, b, meshControl(l, a, b, panelAt), meshLabelAt(l, focus)), text,
            when (l.kind) {
                MeshKind.NONE -> c.destructive
                MeshKind.RELAY -> c.warningText
                MeshKind.DIRECT -> c.foreground
            },
            1f,
        )
    }
    val glyphs = nodes.map { n ->
        val u = n.usageInfo()
        val sub = mapStatusLine(n).ifEmpty { if (u != null) stringResource(R.string.map_usage, pct(u.cpu), pct(u.memory)) else "" }
        MapGlyph(n, sub, if (dim(n.id)) 0.4f else 1f, hover = focus == n.id)
    }
    Spacer(Modifier.height(16.dp))
    MapCanvas(
        layout, avail, stringResource(R.string.mesh_title), lines, tags, glyphs,
        panelAlpha = if (dim(MAP_PANEL)) 0.4f else 1f,
        onHover = { hover = it },
        onTap = { id ->
            when {
                id == null -> pinned = null
                id != MAP_PANEL && id == focus -> openNode(id)
                else -> pinned = id
            }
        },
    )
    Spacer(Modifier.height(16.dp))
    MapLegend(
        listOf(
            LegendEntry(stringResource(R.string.map_direct), c.mutedForeground.copy(alpha = 0.6f)),
            LegendEntry(stringResource(R.string.map_relay), c.warning, DashRelay),
            LegendEntry(stringResource(R.string.map_none), c.destructive, DashNone, textColor = c.destructive),
        ),
    )
    Spacer(Modifier.height(16.dp))
    failed()
    val issues = links.filter { it.kind != MeshKind.DIRECT }
    if (issues.isNotEmpty()) {
        SectionDivider()
        SectionHeader(stringResource(R.string.mesh_issues))
        // 悬停或点选一行时在图上突出这条连接。
        for (l in issues) {
            val end = if (l.a == MAP_PANEL) l.b else l.a
            LinkRow(
                l, snap.statuses, names, selected = pinned == end,
                onClick = { pinned = if (pinned == end) null else end },
                modifier = Modifier.mouseHover(onMove = { hover = end }, onExit = { hover = null }),
            )
        }
    }
    if (live.size >= 2) {
        SectionDivider()
        LatencyMatrix(vm, live, snap, names, avail)
    }
}

/** 直连延迟的底色，越慢越深。只用灰度，有问题的连接（中转、不可达）才带颜色。 */
@Composable
private fun heat(rtt: Double): Color {
    val f = NagiTheme.colors.foreground
    return when {
        rtt < 10 -> f.copy(alpha = 0.04f)
        rtt < 50 -> f.copy(alpha = 0.08f)
        rtt < 150 -> f.copy(alpha = 0.14f)
        else -> f.copy(alpha = 0.22f)
    }
}

/**
 * 节点两两之间的当前延迟，每格是行所在节点测到列所在节点的值（同网页的 LatencyMatrix）。
 * 点选一格，下面显示这两个节点之间两个方向的延迟历史；放不下时横向滚动。
 */
@Composable
private fun LatencyMatrix(vm: MeshViewModel, nodes: List<Node>, snap: MeshSnapshot, names: Names, avail: Dp) {
    val c = NagiTheme.colors
    val cells = meshCells(snap.statuses)
    var pick by rememberSaveable { mutableStateOf<String?>(null) }
    val picked = pick?.split("|")?.takeIf { p -> p.size == 2 && p.all { id -> nodes.any { it.id == id } } }
    val gap = 4.dp
    val head = 112.dp
    val cell = maxOf(84.dp, (avail - head - gap * nodes.size) / nodes.size)
    SectionHeader(stringResource(R.string.mesh_latency_title))
    Column(Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(gap)) {
        Row(horizontalArrangement = Arrangement.spacedBy(gap)) {
            Spacer(Modifier.width(head))
            for (n in nodes) {
                Text(n.name, style = NagiTheme.type.secondary, color = c.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis, textAlign = TextAlign.Center, modifier = Modifier.width(cell))
            }
        }
        for (from in nodes) {
            Row(horizontalArrangement = Arrangement.spacedBy(gap), verticalAlignment = Alignment.CenterVertically) {
                Text(from.name, style = NagiTheme.type.secondary, color = c.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.width(head).padding(end = 8.dp))
                for (to in nodes) {
                    if (from.id == to.id) {
                        Spacer(Modifier.width(cell).height(36.dp))
                        continue
                    }
                    val v = cells["${from.id}|${to.id}"]
                    val on = picked != null && ((picked[0] == from.id && picked[1] == to.id) || (picked[0] == to.id && picked[1] == from.id))
                    val relay = v != null && meshRank(v.path) > 0
                    val text = when {
                        v != null && v.rtt > 0 -> formatMs(v.rtt)
                        relay -> stringResource(if (v!!.path == "none") R.string.map_none else R.string.map_relay)
                        else -> "—"
                    }
                    val color = when {
                        v?.path == "none" -> c.destructive
                        relay -> c.warningText
                        v != null && v.rtt > 0 -> c.foreground
                        else -> c.mutedForeground
                    }
                    val shape = RoundedCornerShape(6.dp)
                    Box(
                        Modifier.width(cell).height(36.dp).clip(shape)
                            .background(if (v != null && v.rtt > 0) heat(v.rtt) else c.muted.copy(alpha = 0.4f))
                            .then(if (on) Modifier.border(1.5.dp, c.foreground, shape) else Modifier)
                            .clickable(role = Role.Button) { pick = if (on) null else "${from.id}|${to.id}" },
                        contentAlignment = Alignment.Center,
                    ) {
                        Text(text, style = NagiTheme.type.secondary.copy(fontFeatureSettings = "tnum"), color = color, maxLines = 1)
                    }
                }
            }
        }
    }
    Spacer(Modifier.height(12.dp))
    val ends = picked?.let { (a, b) ->
        val ka = snap.statuses[a]?.publicKey ?: ByteString.EMPTY
        val kb = snap.statuses[b]?.publicKey ?: ByteString.EMPTY
        HistoryEnds(a, a, ka, b, b, kb).takeIf { !ka.isEmpty && !kb.isEmpty }
    }
    Column(Modifier.fillMaxWidth().autoHeight()) {
        if (ends != null) LatencyChart(vm.repo, ends, names)
        else Text(stringResource(R.string.mesh_latency_hint), style = NagiTheme.type.body, color = c.mutedForeground)
    }
}
