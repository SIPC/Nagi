package ink.sipc.nagi.nodes

import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ink.sipc.nagi.data.Access
import ink.sipc.nagi.data.NodeMetrics
import ink.sipc.nagi.data.Polled
import ink.sipc.nagi.data.TrafficRange
import ink.sipc.nagi.data.usageInfo
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.proto.v1.ProjectSummary
import ink.sipc.nagi.ui.Banner
import ink.sipc.nagi.ui.ChartLegend
import ink.sipc.nagi.ui.ChartLine
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.EmptyState
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.KeyValueRow
import ink.sipc.nagi.ui.LineChart
import ink.sipc.nagi.ui.ListRow
import ink.sipc.nagi.ui.Motion
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiIconButton
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.NagiTopBar
import ink.sipc.nagi.ui.PageTabs
import ink.sipc.nagi.ui.Roll
import ink.sipc.nagi.ui.SectionDivider
import ink.sipc.nagi.ui.SectionHeader
import ink.sipc.nagi.ui.Skeleton
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.StatusDot
import ink.sipc.nagi.ui.UnderlineTabs
import ink.sipc.nagi.ui.autoHeight
import ink.sipc.nagi.ui.formatBytes
import ink.sipc.nagi.ui.formatBytesText
import ink.sipc.nagi.ui.formatRate
import ink.sipc.nagi.ui.formatRateText
import ink.sipc.nagi.ui.rowClickable
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

// 节点页（docs/android.md「节点页」，设计稿 s7 第 2 张）：和容器页同一种写法。顶部是状态和地址，标签是监控、容器、
// 镜像（终端和文件以后再做），节点信息列在监控的最下面。重命名、移除在 ⋯ 里。

/** 节点页上要跳去的地方，交给 App 的导航。 */
interface NodeActions {
    fun back()
    fun openContainer(projectId: String, name: String)

    /** 节点被移除了（回到节点列表）。 */
    fun removed()
}

/**
 * 一个节点的详情。[showBack] 为 false 时（宽屏右边一栏的第一页）顶部栏没有返回。
 * 节点来自节点列表（[list]），监控和容器在自己的 ViewModel 里查。
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NodeScreen(list: NodesViewModel, nodeId: String, actions: NodeActions, showBack: Boolean = true) {
    val state by list.list.collectAsStateWithLifecycle()
    val ready = state as? Polled.Ready
    val node = ready?.value?.nodes?.firstOrNull { it.id == nodeId }
    val access = ready?.value?.access
    val canManage = access?.can(Access.NODE_MANAGE) == true
    var menu by remember { mutableStateOf(false) }
    var dialog by remember { mutableStateOf<String?>(null) }
    val title = node?.name ?: stringResource(R.string.nodes_title)
    Column(Modifier.fillMaxSize().navigationBarsPadding()) {
        val more: @Composable androidx.compose.foundation.layout.RowScope.() -> Unit = {
            if (node != null && canManage) NagiIconButton(NagiIcons.More, stringResource(R.string.more), { menu = true })
        }
        if (showBack) NagiBackTopBar(title, onBack = actions::back, actions = more) else NagiTopBar(title, actions = more)
        Box(Modifier.weight(1f).fillMaxWidth()) {
        when {
            state is Polled.Loading -> SkeletonRows(4, Modifier.padding(horizontal = Dimens.pageHorizontal))
            state is Polled.Failed -> InlineError((state as Polled.Failed).message, stringResource(R.string.retry), list::refresh, Modifier.padding(horizontal = Dimens.pageHorizontal))
            node == null -> EmptyState(stringResource(R.string.node_not_found))
            else -> NodeBody(list, node, access!!, actions)
        }
        // 手机上节点页盖住了一级页面，操作结果在这里提示；宽屏由外层的 Snackbar 提示。
        if (showBack) {
            val snackbar = remember { SnackbarHostState() }
            LaunchedEffect(list) { list.messages.collect { snackbar.showSnackbar(it) } }
            SnackbarHost(snackbar, Modifier.align(Alignment.BottomCenter)) {
                Snackbar(it, containerColor = NagiTheme.colors.primary, contentColor = NagiTheme.colors.primaryForeground)
            }
        }
        }
    }
    if (menu && node != null) {
        ModalBottomSheet(onDismissRequest = { menu = false }, containerColor = NagiTheme.colors.card) {
            Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 12.dp)) {
                Text(node.name, style = NagiTheme.type.section, color = NagiTheme.colors.foreground, modifier = Modifier.padding(bottom = 8.dp))
                SheetRow(stringResource(R.string.rename)) { menu = false; dialog = "rename" }
                SheetRow(stringResource(R.string.remove_node), NagiTheme.colors.destructive) { menu = false; dialog = "remove" }
            }
        }
    }
    if (node != null) {
        when (dialog) {
            "rename" -> RenameDialog(list, node) { dialog = null }
            "remove" -> RemoveDialog(list, node, onDone = { dialog = null; actions.removed() }) { dialog = null }
        }
    }
}

/** 底部弹出面板里的一项操作。 */
@Composable
internal fun SheetRow(text: String, color: Color = NagiTheme.colors.foreground, enabled: Boolean = true, hint: String? = null, onClick: () -> Unit) {
    Column(Modifier.fillMaxWidth().rowClickable(onClick, enabled = enabled).padding(vertical = 14.dp)) {
        Text(text, style = NagiTheme.type.body, color = if (enabled) color else NagiTheme.colors.mutedForeground)
        if (hint != null) Text(hint, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
    }
}

/** 节点页的标签。终端和文件只给有「节点终端和文件」权限的成员（docs/node-access.md#权限）。 */
private enum class NodeTab { METRICS, CONTAINERS, IMAGES, TERMINAL, FILES }

@Composable
private fun NodeBody(list: NodesViewModel, n: Node, access: Access, actions: NodeActions) {
    val vm: NodeViewModel = viewModel(key = "node-${n.id}", factory = Factory { NodeViewModel(list.repo, n.id) })
    var tab by rememberSaveable(n.id) { mutableStateOf(NodeTab.METRICS) }
    // 文件页的「在此打开终端」：带着目录切到终端。
    var shellCwd by rememberSaveable(n.id) { mutableStateOf("") }
    var approve by remember { mutableStateOf(false) }
    if (n.pending) {
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(bottom = 24.dp)) {
            Column(Modifier.padding(horizontal = Dimens.pageHorizontal)) {
                Spacer(Modifier.height(Dimens.topBarToContent))
                StatusLine(n)
            }
            Spacer(Modifier.height(16.dp))
            // 待批准：批准前不能查看和操作（和网页一样），写明节点 ID 和来源地址，同名的节点靠它们分清。
            Banner(
                stringResource(R.string.pending), dot = NagiTheme.colors.warning,
                lines = listOf(
                    stringResource(R.string.pending_notice),
                    stringResource(R.string.pending_id, n.id),
                    if (n.address.isNotEmpty()) stringResource(R.string.pending_address, n.address) else "",
                ),
                action = if (access.can(Access.NODE_MANAGE)) stringResource(R.string.approve) else null, onAction = { approve = true },
            )
            if (approve) ApproveDialog(list, n) { approve = false }
        }
        return
    }
    // 镜像要「节点镜像」，节点的版本支持镜像管理时才有（和网页一样）。
    val images = access.can(Access.NODE_IMAGES) && n.info.imageOps
    val root = access.can(Access.NODE_ROOT)
    val tabs = listOfNotNull(
        NodeTab.METRICS, NodeTab.CONTAINERS, NodeTab.IMAGES.takeIf { images },
        NodeTab.TERMINAL.takeIf { root }, NodeTab.FILES.takeIf { root },
    )
    if (tab !in tabs) tab = NodeTab.METRICS
    val labels = tabs.map {
        stringResource(
            when (it) {
                NodeTab.METRICS -> R.string.tab_metrics
                NodeTab.CONTAINERS -> R.string.tab_containers
                NodeTab.IMAGES -> R.string.images
                NodeTab.TERMINAL -> R.string.tab_terminal
                NodeTab.FILES -> R.string.tab_files
            },
        )
    }
    val header: @Composable () -> Unit = {
        Column(Modifier.padding(horizontal = Dimens.pageHorizontal)) {
            Spacer(Modifier.height(Dimens.topBarToContent))
            StatusLine(n)
        }
        if (n.isolated) {
            Spacer(Modifier.height(16.dp))
            Banner(stringResource(R.string.isolated), dot = NagiTheme.colors.destructive, lines = listOf(stringResource(R.string.isolated_hint)))
        }
        Spacer(Modifier.height(12.dp))
        PageTabs(labels, tabs.indexOf(tab), { tab = tabs[it] }, Modifier.fillMaxWidth())
    }
    // 终端和文件铺满标签以下的高度（自己滚动），其余的标签和标题一起滚动。
    if (tab == NodeTab.TERMINAL || tab == NodeTab.FILES) {
        Column(Modifier.fillMaxSize()) {
            header()
            val blocked = nodeAccessBlock(n)
            Box(Modifier.weight(1f).fillMaxWidth()) {
                when {
                    !n.online -> OfflineNote()
                    blocked != null -> blocked()
                    tab == NodeTab.TERMINAL -> TerminalTab(list.repo.session, n, shellCwd)
                    else -> FilesTab(list.repo.session, n, onTerminal = { shellCwd = it; tab = NodeTab.TERMINAL }, showMessage = list::message)
                }
            }
        }
        return
    }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(bottom = 24.dp)) {
        header()
        AnimatedContent(
            targetState = tab,
            transitionSpec = { fadeIn(tween(Motion.FADE_IN, easing = Motion.Enter)) togetherWith fadeOut(tween(100, easing = Motion.Exit)) },
            label = "node-tab",
        ) { t ->
            Column(Modifier.fillMaxWidth().padding(horizontal = Dimens.pageHorizontal)) {
                when (t) {
                    NodeTab.CONTAINERS -> ContainersTab(vm, n, actions)
                    NodeTab.IMAGES -> ImagesTab(list, n.id, n.name)
                    else -> MetricsTab(vm, n, access)
                }
            }
        }
    }
}

/** 节点离线时终端和文件用不了。 */
@Composable
private fun OfflineNote() {
    Column(Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 16.dp)) {
        Text(stringResource(R.string.offline_title), style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground)
        Text(stringResource(R.string.offline_notice), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
    }
}

/** 终端和文件用不了的原因：节点在本地关闭了节点访问、版本过低（和网页的 nodeAccessBlock 一样）。能用时为 null。 */
private fun nodeAccessBlock(n: Node): (@Composable () -> Unit)? = when {
    n.info.nodeAccessDisabled -> {
        {
            val copy = rememberCopy()
            Column(Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 16.dp)) {
                Text(stringResource(R.string.node_access_disabled), style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground)
                Text(stringResource(R.string.node_access_disabled_text), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
                Spacer(Modifier.height(8.dp))
                Text(
                    "nagi node access enable", style = NagiTheme.type.mono, color = NagiTheme.colors.foreground,
                    modifier = Modifier.fillMaxWidth().rowClickable({ copy("nagi node access enable") }).padding(vertical = 8.dp),
                )
            }
        }
    }
    !n.info.nodeAccess -> {
        { Text(stringResource(R.string.node_access_old), style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 16.dp)) }
    }
    else -> null
}

/** 状态、地址、面板内置：「● 在线 · 203.0.113.12」。地址只给管理员。 */
@Composable
private fun StatusLine(n: Node) {
    val c = NagiTheme.colors
    val status = when {
        n.pending -> stringResource(R.string.pending)
        n.online -> stringResource(R.string.online)
        else -> nodeStatusText(n)
    }
    Row(verticalAlignment = Alignment.CenterVertically) {
        StatusDot(nodeDot(n))
        Spacer(Modifier.width(6.dp))
        val parts = listOfNotNull(
            status,
            n.address.takeIf { it.isNotEmpty() },
            stringResource(R.string.embedded).takeIf { n.info.embedded },
        )
        Text(parts.joinToString(" · "), style = NagiTheme.type.secondary, color = c.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis)
    }
}

// ---- 监控 ----

/** 四个数值块：CPU、内存、磁盘、流量。 */
private enum class Tile { CPU, MEM, DISK, NET }

@Composable
private fun ColumnScope.MetricsTab(vm: NodeViewModel, n: Node, access: Access) {
    if (!n.online) {
        Spacer(Modifier.height(16.dp))
        Text(stringResource(R.string.offline_title), style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground)
        Text(stringResource(R.string.offline_notice), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
    } else {
        Metrics(vm, n)
    }
    if (access.can(Access.NODE_DETAILS) && n.hasInfo()) {
        SectionDivider()
        NodeInfo(n)
    }
}

@Composable
private fun Metrics(vm: NodeViewModel, n: Node) {
    val state by vm.metrics.collectAsStateWithLifecycle()
    val range by vm.range.collectAsStateWithLifecycle()
    // 换时间范围时先留着上一份，新的到了再换（和网页的 placeholderData 一样），不闪回骨架。
    val last = remember { mutableStateOf<NodeMetrics?>(null) }
    (state as? Polled.Ready)?.let { last.value = it.value }
    val ranges = TrafficRange.entries
    Spacer(Modifier.height(12.dp))
    UnderlineTabs(
        listOf(stringResource(R.string.range_hour), stringResource(R.string.range_day), stringResource(R.string.range_week)),
        selected = ranges.indexOf(range), onSelect = { vm.setRange(ranges[it]) }, spacing = 20.dp,
    )
    Spacer(Modifier.height(12.dp))
    val m = last.value
    val failed = state as? Polled.Failed
    when {
        m != null -> MetricTiles(m, n, range)
        failed != null -> InlineError(failed.message, stringResource(R.string.retry), vm::retryMetrics, height = 120.dp)
        else -> TilesSkeleton()
    }
}

@Composable
private fun TilesSkeleton() {
    Column(verticalArrangement = Arrangement.spacedBy(16.dp)) {
        repeat(2) {
            Row(horizontalArrangement = Arrangement.spacedBy(32.dp)) {
                repeat(2) {
                    Column(Modifier.weight(1f)) {
                        Skeleton(Modifier.width(48.dp).height(12.dp))
                        Spacer(Modifier.height(8.dp))
                        Skeleton(Modifier.width(72.dp).height(18.dp))
                        Spacer(Modifier.height(8.dp))
                        Skeleton(Modifier.fillMaxWidth().height(28.dp))
                    }
                }
            }
        }
    }
}

/**
 * 数值块（docs/android.md「数值块展开」）：名称、当前值、小折线。点一个数值块，它变成选中的底色，在这一组下面展开完整的图表；
 * 再点收起，点另一个换成它的图表。手机上两个一行，宽的时候（480 以上）四个一行。
 */
@Composable
private fun MetricTiles(m: NodeMetrics, n: Node, range: TrafficRange) {
    var open by rememberSaveable { mutableStateOf<Tile?>(null) }
    val c = NagiTheme.colors
    val cpus = n.info.cpus
    val memTotal = n.info.memoryBytes.toDouble().coerceAtLeast(0.0)
    val diskTotal = n.usageInfo()?.diskTotal ?: n.info.diskBytes.toDouble().coerceAtLeast(0.0)
    // CPU 按核数折成 100%（和节点列表、首页的占用环一样）。
    val cpu = m.values["node:cpu"].orEmpty().map { if (cpus > 0) it / cpus else it }
    val mem = m.values["node:mem"].orEmpty()
    val disk = m.values["node:disk"].orEmpty()
    val rx = m.values["node:net_rx"].orEmpty()
    val tx = m.values["node:net_tx"].orEmpty()
    val times = listOf("node:cpu", "node:mem", "node:net_rx").firstNotNullOfOrNull { k -> m.times[k]?.takeIf { it.isNotEmpty() } }.orEmpty()
    fun ofTotal(v: Double, total: Double): Pair<String, String> =
        if (total > 0) formatBytes(v / 100 * total).let { (a, b) -> "$a $b" to "/ ${formatBytesText(total)}" } else percentText(v) to ""
    val cpuLast = cpu.lastOrNull()
    val memLast = mem.lastOrNull()
    val diskLast = disk.lastOrNull()
    val netLast = if (rx.isNotEmpty() && tx.isNotEmpty()) rx.last() + tx.last() else null
    val dash = "—" to ""
    val values = mapOf(
        Tile.CPU to (cpuLast?.let { percentText(it) to "" } ?: dash),
        Tile.MEM to (memLast?.let { ofTotal(it, memTotal) } ?: dash),
        Tile.DISK to (diskLast?.let { ofTotal(it, diskTotal) } ?: dash),
        Tile.NET to (netLast?.let { formatRate(it) } ?: dash),
    )
    val fractions = mapOf(Tile.CPU to (cpuLast ?: 0.0) / 100, Tile.MEM to (memLast ?: 0.0) / 100, Tile.DISK to (diskLast ?: 0.0) / 100, Tile.NET to 0.0)
    val labels = mapOf(Tile.CPU to "CPU", Tile.MEM to stringResource(R.string.memory), Tile.DISK to stringResource(R.string.disk), Tile.NET to stringResource(R.string.traffic))
    val rxLabel = stringResource(R.string.net_rx)
    val txLabel = stringResource(R.string.net_tx)
    val lines = { t: Tile ->
        when (t) {
            Tile.CPU -> listOf(ChartLine(cpu.map { it.toFloat() }, c.mutedForeground, "CPU"))
            Tile.MEM -> listOf(ChartLine(mem.map { it.toFloat() }, c.mutedForeground, labels[Tile.MEM]!!))
            Tile.DISK -> listOf(ChartLine(disk.map { it.toFloat() }, c.mutedForeground, labels[Tile.DISK]!!))
            // 两条线用深浅两档灰区分，都是正常数据（同网页）。
            Tile.NET -> listOf(ChartLine(rx.map { it.toFloat() }, c.mutedForeground, rxLabel), ChartLine(tx.map { it.toFloat() }, c.foreground.copy(alpha = 0.6f), txLabel))
        }
    }
    val format = { t: Tile ->
        { v: Float ->
            when (t) {
                Tile.CPU -> percentText(v.toDouble())
                Tile.MEM -> if (memTotal > 0) formatBytesText(v / 100.0 * memTotal) else percentText(v.toDouble())
                Tile.DISK -> if (diskTotal > 0) formatBytesText(v / 100.0 * diskTotal) else percentText(v.toDouble())
                Tile.NET -> formatRateText(v.toDouble())
            }
        }
    }
    BoxWithConstraints(Modifier.fillMaxWidth()) {
        val perRow = if (maxWidth >= 480.dp) 4 else 2
        Column(Modifier.fillMaxWidth().autoHeight()) {
            Tile.entries.chunked(perRow).forEach { row ->
                Row(Modifier.fillMaxWidth().padding(vertical = 4.dp), horizontalArrangement = Arrangement.spacedBy(32.dp)) {
                    row.forEach { t ->
                        val (value, suffix) = values[t]!!
                        MetricTile(
                            labels[t]!!, value, suffix, usageTextColor(fractions[t]!!, c.foreground), lines(t), t == open,
                            Modifier.weight(1f),
                        ) { open = if (open == t) null else t }
                    }
                }
            }
            val t = open
            if (t != null) {
                Spacer(Modifier.height(12.dp))
                if (t == Tile.NET) {
                    ChartLegend(listOf(rxLabel to c.mutedForeground, txLabel to c.foreground.copy(alpha = 0.6f)))
                    Spacer(Modifier.height(8.dp))
                }
                val tip = remember(range) { SimpleDateFormat(if (range == TrafficRange.WEEK) "M/d HH:mm" else "HH:mm", Locale.getDefault()) }
                LineChart(
                    lines(t), Modifier.fillMaxWidth(), xLabels = timeLabels(times, range),
                    tipTime = { i -> times.getOrNull(i)?.let { tip.format(Date(it * 1000)) } ?: "" },
                    tipFormat = format(t),
                )
            }
        }
    }
}

@Composable
private fun MetricTile(label: String, value: String, suffix: String, color: Color, lines: List<ChartLine>, selected: Boolean, modifier: Modifier, onClick: () -> Unit) {
    val c = NagiTheme.colors
    Column(modifier.rowClickable(onClick, highlight = if (selected) c.muted else null).padding(vertical = 8.dp)) {
        Text(label, style = NagiTheme.type.secondary, color = c.mutedForeground)
        Spacer(Modifier.height(2.dp))
        Row(verticalAlignment = Alignment.Bottom) {
            Roll(value, NagiTheme.type.value, color)
            if (suffix.isNotEmpty()) Roll(" $suffix", NagiTheme.type.secondary, c.mutedForeground, Modifier.padding(bottom = 3.dp))
        }
        Spacer(Modifier.height(8.dp))
        LineChart(lines, Modifier.fillMaxWidth(), height = 28.dp)
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
    return (0 until 5).map { i -> if (i == 4) now else fmt.format(Date((first + (last - first) * i / 4) * 1000)) }
}

/** 节点信息（要「查看节点详情」）：主机名、系统、配置、Docker、Nagi 的版本，和网页节点页标题下的一行一样。 */
@Composable
private fun NodeInfo(n: Node) {
    val i = n.info
    SectionHeader(stringResource(R.string.node_info))
    val w = 72.dp
    if (i.hostname.isNotEmpty()) KeyValueRow(stringResource(R.string.hostname), i.hostname, mono = true, labelWidth = w)
    if (i.os.isNotEmpty()) KeyValueRow(stringResource(R.string.system), "${i.os}/${i.arch}", labelWidth = w)
    val spec = listOfNotNull(
        i.cpus.takeIf { it > 0 }?.let { stringResource(R.string.cores, it) },
        i.memoryBytes.takeIf { it > 0 }?.let { formatBytesText(it.toDouble()) },
    ).joinToString(" · ")
    if (spec.isNotEmpty()) KeyValueRow(stringResource(R.string.spec), spec, labelWidth = w)
    val disk = n.usageInfo()?.diskTotal?.takeIf { it > 0 } ?: i.diskBytes.toDouble().takeIf { it > 0 }
    if (disk != null) KeyValueRow(stringResource(R.string.disk), formatBytesText(disk), labelWidth = w)
    if (i.dockerVersion.isNotEmpty()) {
        KeyValueRow("Docker", i.dockerVersion + if (i.composeVersion.isNotEmpty()) " · Compose ${i.composeVersion}" else "", labelWidth = w)
    }
    if (i.version.isNotEmpty()) KeyValueRow("Nagi", i.version, labelWidth = w)
}

// ---- 容器 ----

/** 运行在这个节点上的容器（来自面板的项目列表）。节点离线时状态未知，列表仍然可以看。 */
@Composable
private fun ContainersTab(vm: NodeViewModel, n: Node, actions: NodeActions) {
    val state by vm.projects.collectAsStateWithLifecycle()
    Spacer(Modifier.height(8.dp))
    Column(Modifier.fillMaxWidth().autoHeight()) {
        when (val s = state) {
            Polled.Loading -> SkeletonRows(3)
            is Polled.Failed -> InlineError(s.message, stringResource(R.string.retry), vm::retryProjects)
            is Polled.Ready -> {
                val rows = s.value.flatMap { p -> p.project.containersList.filter { it.nodeId == n.id }.map { p to it } }
                if (rows.isEmpty()) {
                    Text(stringResource(R.string.no_containers), style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(vertical = 14.dp))
                }
                rows.forEach { (p, c) ->
                    val st = containerState(p, c.name, c.deployedVersion > 0, n.online)
                    val version = if (c.deployedVersion > 0) "v${c.deployedVersion}" else ""
                    ListRow(
                        c.name, detail = listOf(p.project.name, version).filter { it.isNotEmpty() }.joinToString(" · "), dot = st.second,
                        onClick = { actions.openContainer(p.project.id, c.name) },
                        trailing = { Text(st.first, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, maxLines = 1) },
                    )
                }
            }
        }
    }
}

/** 容器的状态文字和颜色，规则和网页的 stateOf 一样；节点离线时状态未知。 */
@Composable
private fun containerState(p: ProjectSummary, name: String, deployed: Boolean, online: Boolean): Pair<String, Color> {
    val c = NagiTheme.colors
    if (!online) return stringResource(R.string.state_unknown) to c.mutedForeground
    if (!deployed) return stringResource(R.string.state_not_deployed) to c.mutedForeground
    val svcs = p.status.servicesList.filter { it.container == name }
    val total = svcs.size
    val running = svcs.count { it.state == "running" }
    return when {
        svcs.any { it.health == "unhealthy" } -> stringResource(R.string.state_unhealthy) to c.destructive
        svcs.any { it.state == "dead" } -> stringResource(R.string.state_abnormal) to c.destructive
        svcs.any { it.state == "restarting" } -> stringResource(R.string.state_restarting) to c.warning
        total > 0 && svcs.all { it.state == "paused" } -> stringResource(R.string.state_paused) to c.warning
        total > 0 && running == total -> if (svcs.any { it.health == "starting" }) stringResource(R.string.state_starting) to c.warning else stringResource(R.string.state_running) to c.success
        running > 0 -> stringResource(R.string.state_partial, running, total) to c.warning
        else -> stringResource(R.string.state_stopped) to c.mutedForeground
    }
}

// ---- 重命名、移除 ----

@Composable
private fun RenameDialog(list: NodesViewModel, n: Node, onDismiss: () -> Unit) {
    var name by remember { mutableStateOf(n.name) }
    val action = rememberAction()
    val done = stringResource(R.string.renamed)
    val save = {
        val v = name.trim()
        if (v == n.name) onDismiss()
        else action.run("save") {
            list.repo.rename(n.id, v)
            list.refresh()
            list.message(done)
            onDismiss()
        }
    }
    NagiDialog(
        title = stringResource(R.string.rename_title), text = stringResource(R.string.rename_description),
        confirm = stringResource(R.string.save), cancel = stringResource(R.string.cancel),
        onConfirm = { save() }, onDismiss = onDismiss, busy = action.busy != null, confirmEnabled = name.isNotBlank(),
    ) {
        Spacer(Modifier.height(16.dp))
        NagiTextField(name, { name = it; action.error = null }, label = stringResource(R.string.name), error = action.error != null, maxLength = 64, onDone = { save() })
        if (n.info.hostname.isNotEmpty()) {
            Spacer(Modifier.height(6.dp))
            Text(stringResource(R.string.hostname_hint, n.info.hostname), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
        }
        ActionError(action)
    }
}

@Composable
private fun RemoveDialog(list: NodesViewModel, n: Node, onDone: () -> Unit, onDismiss: () -> Unit) {
    val action = rememberAction()
    val done = stringResource(R.string.node_removed, n.name)
    NagiDialog(
        title = stringResource(R.string.remove_node), text = stringResource(R.string.remove_confirm, n.name),
        confirm = stringResource(R.string.remove), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy != null,
        onDismiss = onDismiss,
        onConfirm = {
            action.run("remove") {
                list.repo.remove(n.id)
                list.message(done)
                list.refresh()
                onDone()
            }
        },
    ) { ActionError(action) }
}
