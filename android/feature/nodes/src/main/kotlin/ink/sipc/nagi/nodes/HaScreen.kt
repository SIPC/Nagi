package ink.sipc.nagi.nodes

import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
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
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import ink.sipc.nagi.data.Access
import ink.sipc.nagi.data.HaEdgeType
import ink.sipc.nagi.data.HaFate
import ink.sipc.nagi.data.HaGroup
import ink.sipc.nagi.data.HaItem
import ink.sipc.nagi.data.HaRow
import ink.sipc.nagi.data.MAP_PANEL
import ink.sipc.nagi.data.MapRepo
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.data.Polled
import ink.sipc.nagi.data.haEdges
import ink.sipc.nagi.data.haShapes
import ink.sipc.nagi.data.haUnprotected
import ink.sipc.nagi.data.mapLayout
import ink.sipc.nagi.data.poll
import ink.sipc.nagi.data.simulateFailure
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.EmptyState
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.ListRow
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.NagiTopBar
import ink.sipc.nagi.ui.SectionDivider
import ink.sipc.nagi.ui.SectionHeader
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.StatusDot
import ink.sipc.nagi.ui.UnderlineTabs
import ink.sipc.nagi.ui.autoHeight
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.stateIn

// 高可用（docs/android.md「高可用」，同网页 node-map.tsx 的高可用图层）：手机上不画拓扑图，上面是选节点的选择项，
// 下面按网页的模拟规则，分组列出这个节点故障时它上面的容器（和把它当作热备、备份节点的容器），每行写去向或原因。
// 可用宽度达到 MapMinWidth 时画拓扑图：箭头从容器所在的节点指向它的顶替节点或备份节点，点一个节点模拟它故障。

/** 部署过的容器：和网页的拓扑图一样每 10 秒查一次项目列表。 */
class HaViewModel(repo: MapRepo) : ViewModel() {
    private val refreshes = Channel<Unit>(Channel.CONFLATED)

    val items: StateFlow<Polled<List<HaItem>>> = poll(10_000, refreshes) { repo.haItems() }
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), Polled.Loading)

    fun retry() {
        refreshes.trySend(Unit)
    }
}

/**
 * 高可用整页（要「查看节点」，以及「高可用」或「备份」，同网页）。[onBack] 为 null 时顶部栏没有返回。
 * [openNode] 打开节点页，[openContainer]（项目 ID、容器名）打开容器页，不给时容器行不可点。
 */
@Composable
fun HaScreen(
    list: NodesViewModel,
    session: PanelSession,
    onBack: (() -> Unit)?,
    openNode: (String) -> Unit,
    openContainer: ((String, String) -> Unit)? = null,
    mapMinWidth: Dp = MapMinWidth,
) {
    val vm: HaViewModel = viewModel(key = "ha-" + session.panel.id, factory = Factory { HaViewModel(MapRepo(session)) })
    val state by list.list.collectAsStateWithLifecycle()
    val title = stringResource(R.string.ha_title)
    Column(Modifier.fillMaxSize().navigationBarsPadding()) {
        if (onBack != null) NagiBackTopBar(title, onBack = onBack) else NagiTopBar(title)
        when (val s = state) {
            Polled.Loading -> SkeletonRows(4, Modifier.padding(horizontal = Dimens.pageHorizontal))
            is Polled.Failed -> InlineError(s.message, stringResource(R.string.retry), list::refresh, Modifier.padding(horizontal = Dimens.pageHorizontal))
            is Polled.Ready -> {
                val access = s.value.access
                when {
                    !access.can(Access.NODE_VIEW) -> EmptyState(stringResource(R.string.nodes_denied))
                    // 高可用画的是顶替和备份，要相应的权限（同网页）。
                    !access.can(Access.PROJECT_HA) && !access.can(Access.PROJECT_BACKUP) -> EmptyState(stringResource(R.string.ha_denied))
                    s.value.nodes.isEmpty() -> EmptyState(stringResource(R.string.nodes_empty))
                    else -> HaBody(vm, s.value.nodes, openNode, openContainer, mapMinWidth)
                }
            }
        }
    }
}

@Composable
private fun HaBody(vm: HaViewModel, nodes: List<Node>, openNode: (String) -> Unit, openContainer: ((String, String) -> Unit)?, mapMinWidth: Dp) {
    val state by vm.items.collectAsStateWithLifecycle()
    BoxWithConstraints(Modifier.fillMaxSize()) {
        val wide = maxWidth >= mapMinWidth
        val avail = maxWidth - Dimens.pageHorizontal * 2
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(bottom = 24.dp)) {
            Spacer(Modifier.height(Dimens.topBarToContent))
            if (wide) {
                Column(Modifier.padding(horizontal = Dimens.pageHorizontal)) { WideHa(vm, state, nodes, avail, openNode, openContainer) }
            } else {
                PhoneHa(vm, state, nodes, openNode, openContainer)
            }
        }
    }
}

// ---- 手机：选节点、分组列出 ----

@Composable
private fun PhoneHa(vm: HaViewModel, state: Polled<List<HaItem>>, nodes: List<Node>, openNode: (String) -> Unit, openContainer: ((String, String) -> Unit)?) {
    // 待批准的节点上没有容器，不列。
    val choices = nodes.filter { !it.pending }
    var picked by rememberSaveable { mutableStateOf<String?>(null) }
    val node = choices.firstOrNull { it.id == picked } ?: choices.firstOrNull()
    if (node == null) {
        EmptyState(stringResource(R.string.nodes_empty))
        return
    }
    // 节点多了横向滚动，左右留出页面的边距。
    UnderlineTabs(
        choices.map { it.name }, selected = choices.indexOf(node), onSelect = { picked = choices[it].id },
        modifier = Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = Dimens.pageHorizontal),
        spacing = 20.dp,
    )
    Column(Modifier.fillMaxWidth().padding(horizontal = Dimens.pageHorizontal).autoHeight()) {
        Spacer(Modifier.height(8.dp))
        Report(vm, state, node, nodes, openNode, openContainer)
    }
}

/** 模拟 [node] 故障的结果：标题和「查看节点」，下面按结果分组，最严重的在前。 */
@Composable
private fun Report(vm: HaViewModel, state: Polled<List<HaItem>>, node: Node, nodes: List<Node>, openNode: (String) -> Unit, openContainer: ((String, String) -> Unit)?) {
    val c = NagiTheme.colors
    SectionHeader(stringResource(R.string.ha_if_fails, node.name)) {
        NagiButton(stringResource(R.string.map_open_node), { openNode(node.id) }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
    }
    when (state) {
        Polled.Loading -> SkeletonRows(2)
        is Polled.Failed -> InlineError(state.message, stringResource(R.string.retry), vm::retry)
        is Polled.Ready -> {
            val groups = remember(node.id, nodes, state.value) { simulateFailure(node.id, nodes, state.value) }
            if (groups.isEmpty()) {
                Text(stringResource(R.string.ha_empty), style = NagiTheme.type.body, color = c.mutedForeground, modifier = Modifier.padding(vertical = 8.dp))
            }
            val names = nodes.associate { it.id to it.name }
            groups.forEachIndexed { i, (group, rows) ->
                if (i > 0) Spacer(Modifier.height(12.dp))
                GroupTitle(group)
                for (r in rows) {
                    val ct = r.item.container
                    ListRow(
                        ct.name,
                        detail = listOf(r.item.projectName, fateText(r, names)).filter { it.isNotEmpty() }.joinToString(" · "),
                        onClick = openContainer?.let { open -> { open(r.item.projectId, ct.name) } },
                    )
                }
            }
        }
    }
}

/** 组的颜色：同网页 Status 的语气。 */
@Composable
private fun groupColor(g: HaGroup): Pair<Color, Color> {
    val c = NagiTheme.colors
    return when (g) {
        HaGroup.LOST -> c.destructive to c.destructive
        HaGroup.CONFIRM, HaGroup.BACKUP -> c.warning to c.warningText
        HaGroup.AUTO -> c.success to c.foreground
        HaGroup.REDEPLOY, HaGroup.COLLATERAL -> c.mutedForeground to c.foreground
    }
}

/** 组的标题：「● 自动顶替」，跟在文字里的状态点到文字 6。 */
@Composable
private fun GroupTitle(g: HaGroup) {
    val (dot, text) = groupColor(g)
    val label = when (g) {
        HaGroup.LOST -> R.string.ha_group_lost
        HaGroup.CONFIRM -> R.string.ha_group_confirm
        HaGroup.BACKUP -> R.string.ha_group_backup
        HaGroup.AUTO -> R.string.ha_group_auto
        HaGroup.REDEPLOY -> R.string.ha_group_redeploy
        HaGroup.COLLATERAL -> R.string.ha_group_collateral
    }
    Row(Modifier.padding(top = 8.dp, bottom = 2.dp), verticalAlignment = Alignment.CenterVertically) {
        StatusDot(dot)
        Spacer(Modifier.width(6.dp))
        Text(stringResource(label), style = NagiTheme.type.bodyMedium, color = text)
    }
}

/** 每行的去向或原因。 */
@Composable
private fun fateText(r: HaRow, names: Map<String, String>): String {
    fun name(id: String) = names[id] ?: id
    val sep = stringResource(R.string.list_separator)
    return when (val f = r.fate) {
        HaFate.Lost -> stringResource(R.string.ha_lost)
        is HaFate.StandbyOffline -> stringResource(R.string.ha_standby_offline, name(f.node))
        is HaFate.WitnessOffline -> stringResource(R.string.ha_witness_offline, name(f.node))
        is HaFate.BackupOffline -> stringResource(R.string.ha_backup_offline, f.nodes.joinToString(sep) { name(it) })
        is HaFate.Confirm -> stringResource(R.string.ha_confirm_to, name(f.node))
        is HaFate.Backup -> stringResource(R.string.ha_backup_on, f.nodes.joinToString(sep) { name(it) })
        is HaFate.Auto -> stringResource(R.string.ha_to, name(f.node))
        HaFate.Redeploy -> stringResource(R.string.ha_redeploy)
        is HaFate.Collateral -> stringResource(
            when {
                f.failover && f.backup -> R.string.ha_lose_both
                f.failover -> R.string.ha_lose_failover
                else -> R.string.ha_lose_backup
            },
            name(f.runsOn),
        )
    }
}

// ---- 宽屏：拓扑图 ----

@Composable
private fun WideHa(vm: HaViewModel, state: Polled<List<HaItem>>, nodes: List<Node>, avail: Dp, openNode: (String) -> Unit, openContainer: ((String, String) -> Unit)?) {
    val c = NagiTheme.colors
    var hover by remember { mutableStateOf<String?>(null) }
    var selected by rememberSaveable { mutableStateOf<String?>(null) }
    val items = (state as? Polled.Ready)?.value.orEmpty()
    val ids = nodes.map { it.id }
    val layout = remember(ids, avail) { mapLayout(ids, avail.value) }
    val edges = remember(items, ids) { haEdges(items, ids.toSet()) }
    val shapes = remember(edges, layout) { haShapes(edges, layout.pos) }
    val risky = remember(items) { haUnprotected(items) }
    // 悬停（鼠标）或点选的节点：模拟它故障，突出它的箭头。
    val focus = hover ?: selected
    val neighbors = edges.filter { it.touches(focus) }.flatMap { listOf(it.from, it.to) }.toSet()
    fun dim(id: String) = focus != null && id != focus && id !in neighbors
    val lines = edges.mapNotNull { e ->
        val s = shapes[e.key] ?: return@mapNotNull null
        val out = focus != null && e.from == focus
        MapLine(
            e.key, s.arrow.from, s.arrow.control, s.arrow.tip,
            color = if (out) c.foreground.copy(alpha = 0.8f) else c.mutedForeground.copy(alpha = 0.6f),
            alpha = if (focus != null && !e.touches(focus)) 0.15f else 1f,
            dash = if (e.type == HaEdgeType.BACKUP) DashRelay else null,
            head = s.arrow.head,
        )
    }
    val tags = edges.mapNotNull { e ->
        val s = shapes[e.key] ?: return@mapNotNull null
        MapTag(
            e.key, s.label,
            stringResource(if (e.type == HaEdgeType.FAILOVER) R.string.ha_failover_edge else R.string.ha_backup_edge, e.count),
            if (focus != null && e.from == focus) c.foreground else c.mutedForeground,
            if (focus != null && !e.touches(focus)) 0.15f else 1f,
        )
    }
    val glyphs = nodes.map { n ->
        val status = mapStatusLine(n)
        val k = risky[n.id] ?: 0
        MapGlyph(
            n, status.ifEmpty { stringResource(R.string.containers_count, n.containers) }, if (dim(n.id)) 0.4f else 1f,
            warn = if (status.isEmpty() && k > 0) stringResource(R.string.ha_unprotected, k) else "",
            failed = focus == n.id, hover = hover == n.id,
        )
    }
    Spacer(Modifier.height(16.dp))
    MapCanvas(
        layout, avail, stringResource(R.string.ha_title), lines, tags, glyphs,
        panelAlpha = if (focus != null) 0.4f else 1f,
        onHover = { id -> hover = id?.takeIf { it != MAP_PANEL } },
        onTap = { id -> selected = if (id == null || id == MAP_PANEL || id == selected) null else id },
    )
    Spacer(Modifier.height(16.dp))
    MapLegend(
        listOf(
            LegendEntry(stringResource(R.string.ha_legend_failover), c.mutedForeground.copy(alpha = 0.6f), arrow = true),
            LegendEntry(stringResource(R.string.ha_legend_backup), c.mutedForeground.copy(alpha = 0.6f), DashRelay, arrow = true),
        ),
    )
    Spacer(Modifier.height(8.dp))
    SectionDivider()
    // 图下的故障报告和手机上同一种写法。
    val failed = nodes.firstOrNull { it.id == focus }
    Column(Modifier.fillMaxWidth().autoHeight()) {
        when {
            state is Polled.Failed -> InlineError(state.message, stringResource(R.string.retry), vm::retry)
            state is Polled.Loading -> SkeletonRows(2)
            failed != null -> Report(vm, state, failed, nodes, openNode, openContainer)
            else -> Text(stringResource(R.string.ha_hint), style = NagiTheme.type.body, color = c.mutedForeground)
        }
    }
}
