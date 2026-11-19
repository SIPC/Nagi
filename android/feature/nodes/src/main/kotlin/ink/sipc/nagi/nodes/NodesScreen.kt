package ink.sipc.nagi.nodes

import androidx.compose.animation.core.tween
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
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
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
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ink.sipc.nagi.data.Access
import ink.sipc.nagi.data.NodeList
import ink.sipc.nagi.data.NodeUsageInfo
import ink.sipc.nagi.data.Polled
import ink.sipc.nagi.data.usageInfo
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.ui.Banner
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Collapse
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.EmptyState
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.Motion
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiIconButton
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.Roll
import ink.sipc.nagi.ui.SectionDivider
import ink.sipc.nagi.ui.SettingsRow
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.StatusDot
import ink.sipc.nagi.ui.UsageRing
import ink.sipc.nagi.ui.rowClickable

// 节点列表（docs/android.md「节点列表」，设计稿 s7 第 1 张）：有问题的排在前面；一行是状态点、名称（离线、待批准时后面写状态），
// 第二行写状态、等宽的地址和容器数，右边是 CPU、内存、磁盘三个占用环（和首页的节点状态一样），待批准的是「批准」。

/** 节点列表的权限：还没查到时为 null。 */
fun NodesViewModel.accessOf(state: Polled<NodeList>): Access? = (state as? Polled.Ready)?.value?.access

/**
 * 节点页的一级页面：节点列表。手机上铺满整页（顶部栏和胶囊导航由外层放），宽屏时在左边一栏（[selected] 是右边正在看的节点，
 * 有底色；[header] 为 true 时最上面一行是标题和添加节点，因为宽屏没有顶部栏）。
 */
@Composable
fun NodesContent(
    vm: NodesViewModel,
    contentPadding: PaddingValues,
    onOpen: (String) -> Unit,
    onAdd: () -> Unit,
    showMessage: (String) -> Unit,
    listState: LazyListState = rememberLazyListState(),
    selected: String? = null,
    header: Boolean = false,
    onMesh: () -> Unit = {},
    onHa: () -> Unit = {},
    /** 宽屏右边正在看的是节点网络（mesh）还是高可用（ha），那一行有底色。 */
    view: String? = null,
) {
    val state by vm.list.collectAsStateWithLifecycle()
    val approving by vm.approving.collectAsStateWithLifecycle()
    LaunchedEffect(vm) { vm.messages.collect(showMessage) }
    val access = vm.accessOf(state)
    var confirm by remember { mutableStateOf<Node?>(null) }
    LazyColumn(Modifier.fillMaxSize(), state = listState, contentPadding = contentPadding) {
        if (header) {
            item(key = "header") {
                Row(Modifier.fillMaxWidth().padding(start = Dimens.pageHorizontal, end = 8.dp).heightIn(min = Dimens.iconButton), verticalAlignment = Alignment.CenterVertically) {
                    Text(stringResource(R.string.nodes_title), style = NagiTheme.type.topBar, color = NagiTheme.colors.foreground, modifier = Modifier.weight(1f))
                    if (access?.isAdmin == true) NagiIconButton(NagiIcons.Plus, stringResource(R.string.add_node), onAdd)
                }
            }
        } else {
            item(key = "top") { Spacer(Modifier.height(Dimens.topBarToContent)) }
        }
        when (val s = state) {
            Polled.Loading -> item(key = "loading") { SkeletonRows(4, Modifier.padding(horizontal = Dimens.pageHorizontal)) }
            is Polled.Failed -> item(key = "failed") {
                if (s.unreachable) {
                    Banner(stringResource(R.string.unreachable_title), lines = listOf(s.message), action = stringResource(R.string.retry), onAction = vm::refresh)
                } else {
                    InlineError(s.message, stringResource(R.string.retry), vm::refresh, Modifier.padding(horizontal = Dimens.pageHorizontal))
                }
            }
            is Polled.Ready -> {
                item(key = "unreachable") {
                    Collapse(!s.live) {
                        Banner(stringResource(R.string.unreachable_title), lines = listOf(s.error.orEmpty()), action = stringResource(R.string.retry), onAction = vm::refresh)
                    }
                }
                val list = s.value
                when {
                    !list.access.can(Access.NODE_VIEW) -> item(key = "denied") { EmptyState(stringResource(R.string.nodes_denied), Modifier.padding(top = 48.dp)) }
                    list.nodes.isEmpty() -> item(key = "empty") {
                        EmptyState(stringResource(R.string.nodes_empty), Modifier.padding(top = 48.dp), action = if (list.access.isAdmin) {
                            { NagiButton(stringResource(R.string.add_node), onAdd, variant = ButtonVariant.Outline) }
                        } else {
                            null
                        })
                    }
                    else -> items(list.nodes, key = { it.id }) { n ->
                        NodeRow(
                            n, canApprove = list.access.can(Access.NODE_MANAGE), approving = n.id in approving, selected = n.id == selected,
                            onClick = { onOpen(n.id) }, onApprove = { confirm = n },
                            modifier = Modifier.animateItem(fadeInSpec = tween(Motion.FADE_IN, easing = Motion.Enter), fadeOutSpec = null).padding(horizontal = Dimens.pageHorizontal),
                        )
                    }
                }
                // 列表下面是节点网络和高可用（网页节点页的「网络」「高可用」视图）：手机上是列表，宽屏画拓扑图。
                // 高可用画的是顶替和备份，要相应的权限（和网页一样）。
                if (list.access.can(Access.NODE_VIEW) && list.nodes.isNotEmpty()) item(key = "views") {
                    Column(Modifier.padding(horizontal = Dimens.pageHorizontal)) {
                        SectionDivider()
                        SettingsRow(stringResource(R.string.mesh_title), onMesh, selected = view == "mesh")
                        if (list.access.can(Access.PROJECT_HA) || list.access.can(Access.PROJECT_BACKUP)) {
                            SettingsRow(stringResource(R.string.ha_title), onHa, selected = view == "ha")
                        }
                    }
                }
            }
        }
    }
    confirm?.let { n -> ApproveDialog(vm, n) { confirm = null } }
}

/** 批准前确认（和网页一样写明名称和地址，请确认是自有机器）。 */
@Composable
internal fun ApproveDialog(vm: NodesViewModel, n: Node, onDismiss: () -> Unit) {
    val done = stringResource(R.string.node_approved, n.name)
    val unreachable = stringResource(R.string.not_done_unreachable)
    NagiDialog(
        title = stringResource(R.string.approve_title), text = stringResource(R.string.approve_confirm, n.name, n.address.ifEmpty { n.id }),
        confirm = stringResource(R.string.approve), cancel = stringResource(R.string.cancel),
        onDismiss = onDismiss,
        onConfirm = {
            vm.approve(n, done, unreachable)
            onDismiss()
        },
    )
}

/** 状态点的颜色：在线绿、待批准黄、离线灰（ui.md：离线、状态未知都是灰色）。 */
@Composable
internal fun nodeDot(n: Node) = when {
    n.pending -> NagiTheme.colors.warning
    n.online -> NagiTheme.colors.success
    else -> NagiTheme.colors.mutedForeground
}

/** 名称后面的短状态：待批准、离线 4 分钟；在线时为空。 */
@Composable
internal fun nodeStatusText(n: Node): String = when {
    n.pending -> stringResource(R.string.pending)
    !n.online -> if (n.lastSeen.seconds > 0) stringResource(R.string.node_offline_for, durationText(n.lastSeen.seconds)) else stringResource(R.string.offline)
    else -> ""
}

@Composable
private fun NodeRow(
    n: Node,
    canApprove: Boolean,
    approving: Boolean,
    selected: Boolean,
    onClick: () -> Unit,
    onApprove: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val c = NagiTheme.colors
    val usage = n.usageInfo()
    // 和首页的节点状态同一种写法：第一行名称，第二行状态、地址和容器数，右边是 CPU、内存、磁盘三个占用环。
    Row(
        modifier.fillMaxWidth().rowClickable(onClick, highlight = if (selected) c.muted else null, selectionKey = n.id).heightIn(min = Dimens.rowHeightTwoLine).padding(vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.align(Alignment.Top).padding(top = 7.dp)) { StatusDot(nodeDot(n)) }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(n.name, style = NagiTheme.type.body, color = c.foreground, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
                if (n.info.embedded) {
                    Spacer(Modifier.width(8.dp))
                    Text(stringResource(R.string.embedded), style = NagiTheme.type.secondary, color = c.mutedForeground, maxLines = 1)
                }
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                val status = nodeStatusText(n)
                if (status.isNotEmpty()) {
                    Roll(status, NagiTheme.type.secondary, if (n.pending) c.warningText else c.mutedForeground)
                    Text(" · ", style = NagiTheme.type.secondary, color = c.mutedForeground)
                }
                Text(n.address.ifEmpty { n.id }, style = NagiTheme.type.mono, color = c.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
                if (!n.pending) {
                    Text(" · ", style = NagiTheme.type.secondary, color = c.mutedForeground)
                    Roll(stringResource(R.string.containers_count, n.containers), NagiTheme.type.secondary, c.mutedForeground)
                }
            }
        }
        if (n.pending) {
            if (canApprove) {
                Spacer(Modifier.width(16.dp))
                NagiButton(stringResource(R.string.approve), onApprove, variant = ButtonVariant.Outline, size = ButtonSize.Small, loading = approving)
            }
        } else if (usage != null) {
            val track = if (selected) c.border else c.muted
            Spacer(Modifier.width(16.dp))
            UsageRing("CPU", NagiIcons.Cpu, usage.cpu, track = track)
            Spacer(Modifier.width(8.dp))
            UsageRing(stringResource(R.string.memory), NagiIcons.MemoryStick, usage.memory, track = track)
            Spacer(Modifier.width(8.dp))
            UsageRing(stringResource(R.string.disk), NagiIcons.HardDrive, if (usage.diskTotal > 0 || usage.disk > 0) usage.disk else null, track = track)
        }
    }
}
