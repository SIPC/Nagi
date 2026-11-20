package ink.sipc.nagi.nodes

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.NodesRepo
import ink.sipc.nagi.network.PanelException
import ink.sipc.nagi.network.UnreachableException
import ink.sipc.nagi.network.toPanelException
import ink.sipc.nagi.proto.v1.ImagePullEvent
import ink.sipc.nagi.proto.v1.NodeImage
import ink.sipc.nagi.proto.v1.NodeImages
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.ListRow
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.SwitchLabels
import ink.sipc.nagi.ui.SwitchRow
import ink.sipc.nagi.ui.UnderlineTabs
import ink.sipc.nagi.ui.UsageBar
import ink.sipc.nagi.ui.autoHeight
import ink.sipc.nagi.ui.formatBytesText
import com.connectrpc.Code
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

// 节点的镜像（要「节点镜像」，和网页 pages/node-images.tsx 一样，见 docs/agent.md#节点镜像）：列出镜像，删除没人用的，
// 一键清理，拉取镜像。能不能删由节点判断：有容器使用、Nagi 需要的镜像节点会拒绝，这里只是先置灰。删除和清理要先做高权限确认。
// 镜像是节点页的一个标签。点一行打开这一行的操作（复制 ID、删除标签、删除镜像）。

private enum class Sort { SIZE, CREATED, NAME }

private fun shortId(id: String) = id.removePrefix("sha256:").take(12)
private fun NodeImage.name() = tagsList.firstOrNull().orEmpty()
private fun NodeImage.blocked() = usersCount > 0 || holdsCount > 0

private fun sorted(list: List<NodeImage>, sort: Sort): List<NodeImage> = when (sort) {
    Sort.SIZE -> list.sortedByDescending { it.size }
    Sort.CREATED -> list.sortedByDescending { it.created.seconds }
    // 没有标签的排在最后。
    Sort.NAME -> list.sortedWith(compareBy<NodeImage> { it.name().isEmpty() }.thenBy { it.name() }.thenBy { it.id })
}

private fun NodeImage.matches(q: String): Boolean {
    if (q.isEmpty()) return true
    val fields = listOf(id) + tagsList + digestsList + usersList.flatMap { listOf(it.name, it.projectName, it.container) } + holdsList.flatMap { listOf(it.projectName, it.container) }
    return fields.any { it.lowercase().contains(q) }
}

/** 第二行：使用者，或者 Nagi 需要它的原因，没有时写「未使用」。 */
@Composable
private fun usage(im: NodeImage): String {
    if (im.usersCount > 0) {
        val proxy = stringResource(R.string.nagi_proxy)
        val names = im.usersList.map { u -> if (u.system) proxy else if (u.container.isNotEmpty()) "${u.projectName.ifEmpty { u.projectId }} / ${u.container}" else u.name }.distinct()
        val text = stringResource(R.string.used_by, names.joinToString(stringResource(R.string.list_separator)))
        return if (im.usersList.any { it.running }) text else text + " · " + stringResource(R.string.image_stopped)
    }
    if (im.holdsCount > 0) {
        val reasons = im.holdsList.map { h ->
            val kind = when (h.kind) {
                "deployed" -> stringResource(R.string.hold_deployed)
                "standby" -> stringResource(R.string.hold_standby)
                "backup" -> stringResource(R.string.hold_backup)
                "system" -> stringResource(R.string.nagi_proxy)
                else -> h.kind
            }
            if (h.container.isNotEmpty()) "$kind ${h.projectName.ifEmpty { h.projectId }} / ${h.container}" else kind
        }.distinct()
        return stringResource(R.string.needed_by_nagi, reasons.joinToString(stringResource(R.string.list_separator)))
    }
    return stringResource(R.string.unused)
}

/**
 * 节点页的「镜像」标签（放在节点页的滚动内容里，所以不用 LazyColumn）。上面一行是数量、大小、可释放的空间，
 * 「清理」和「拉取镜像」；下面是筛选、排序和列表。
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun ImagesTab(list: NodesViewModel, nodeId: String, nodeName: String) {
    val repo = list.repo
    // 和网页一样进入标签时查一次，做完操作后再查。
    var data by remember { mutableStateOf<NodeImages?>(null) }
    var loadError by remember { mutableStateOf<String?>(null) }
    var loads by remember { mutableStateOf(0) }
    LaunchedEffect(loads) {
        loadError = null
        try {
            data = repo.images(nodeId)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            loadError = e.message.orEmpty()
        }
    }
    val reload: () -> Unit = { loads++ }
    var sort by rememberSaveable { mutableStateOf(Sort.SIZE) }
    var query by rememberSaveable { mutableStateOf("") }
    var picked by remember { mutableStateOf<NodeImage?>(null) }
    var confirm by remember { mutableStateOf<Pair<String, () -> Unit>?>(null) }
    var pruning by remember { mutableStateOf<NodeImages?>(null) }
    var pulling by remember { mutableStateOf(false) }
    // 删除、清理里没删掉的几项（节点给的原因）。
    var errors by remember { mutableStateOf<List<String>>(emptyList()) }
    val action = rememberAction()
    val context = LocalContext.current

    val all = data?.imagesList.orEmpty()
    val needle = query.trim().lowercase()
    val shown = remember(all, sort, needle) { sorted(all, sort).filter { it.matches(needle) } }
    Column(Modifier.fillMaxWidth()) {
        Spacer(Modifier.height(12.dp))
        Row(Modifier.fillMaxWidth().heightIn(min = 40.dp), verticalAlignment = Alignment.CenterVertically) {
            val d = data
            val total = all.sumOf { it.size }.toDouble()
            val text = if (d == null) "" else listOfNotNull(
                stringResource(R.string.images_summary, all.size, formatBytesText(total)),
                if (d.pruneIdsCount > 0) stringResource(R.string.reclaimable, formatBytesText(d.pruneBytes.toDouble())) else null,
            ).joinToString(" · ")
            Text(text, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.weight(1f))
            Spacer(Modifier.width(12.dp))
            NagiButton(stringResource(R.string.prune), {
                // 清理前重新取一次列表：确认框里列出的就是节点这时算出的。
                action.run("prune-open") {
                    val fresh = repo.images(nodeId)
                    data = fresh
                    pruning = fresh
                }
            }, variant = ButtonVariant.Outline, size = ButtonSize.Small, enabled = d != null && action.busy == null, loading = action.busy == "prune-open")
            Spacer(Modifier.width(12.dp))
            NagiButton(stringResource(R.string.pull_image), { pulling = true }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
        }
        Spacer(Modifier.height(8.dp))
        NagiTextField(query, { query = it }, placeholder = stringResource(R.string.filter_images), keyboardType = KeyboardType.Uri)
        Spacer(Modifier.height(8.dp))
        UnderlineTabs(
            listOf(stringResource(R.string.by_size), stringResource(R.string.by_created), stringResource(R.string.by_name)),
            selected = sort.ordinal, onSelect = { sort = Sort.entries[it] }, spacing = 20.dp,
        )
        Spacer(Modifier.height(4.dp))
        ActionError(action)
        errors.forEach { Text(it, style = NagiTheme.type.secondary, color = NagiTheme.colors.destructive) }
        Column(Modifier.fillMaxWidth().autoHeight()) {
            when {
                data == null && loadError != null -> InlineError(loadError.orEmpty(), stringResource(R.string.retry), { reload() })
                data == null -> SkeletonRows(4)
                shown.isEmpty() -> Text(
                    stringResource(if (all.isEmpty()) R.string.images_empty else R.string.images_no_match),
                    style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(vertical = 14.dp),
                )
                else -> shown.forEach { im ->
                    key(im.id) {
                        val name = im.name()
                        val more = if (im.tagsCount > 1) " " + stringResource(R.string.more_tags, im.tagsCount - 1) else ""
                        ListRow(
                            name.ifEmpty { stringResource(R.string.untagged) } + more,
                            detail = shortId(im.id) + " · " + usage(im),
                            onClick = { picked = im },
                            trailing = { Text(formatBytesText(im.size.toDouble()), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground) },
                        )
                    }
                }
            }
        }
    }

    val remove = { refs: List<ink.sipc.nagi.proto.v1.ImageRef>, tagOnly: Boolean ->
        errors = emptyList()
        action.run("delete") {
            try {
                val res = repo.deleteImages(nodeId, refs)
                val failed = res.resultsList.filter { it.error.isNotEmpty() }
                val ok = res.resultsCount - failed.size
                if (failed.isNotEmpty()) {
                    errors = listOf(context.getString(R.string.delete_failed, failed.size)) + failed.map { r ->
                        val label = r.tag.ifEmpty { data?.imagesList.orEmpty().firstOrNull { it.id == r.id }?.name()?.ifEmpty { null } ?: shortId(r.id) }
                        "$label：${r.error}"
                    }
                }
                if (ok > 0) list.message(if (tagOnly) context.getString(R.string.tag_deleted) else context.getString(R.string.images_deleted, ok))
            } finally {
                reload()
            }
        }
    }

    picked?.let { im ->
        val name = im.name().ifEmpty { shortId(im.id) }
        val copy = rememberCopy()
        val lockHint = if (im.usersCount > 0) stringResource(R.string.in_use_hint) else stringResource(R.string.needed_hint)
        val deleteConfirm = stringResource(R.string.delete_image_confirm, name)
        ModalBottomSheet(onDismissRequest = { picked = null }, containerColor = NagiTheme.colors.card) {
            Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 12.dp)) {
                Text(name, style = NagiTheme.type.section, color = NagiTheme.colors.foreground, maxLines = 2, overflow = TextOverflow.Ellipsis)
                Text(
                    listOf(formatBytesText(im.size.toDouble()), stringResource(R.string.created_at, agoText(im.created.seconds))).joinToString(" · "),
                    style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(bottom = 8.dp),
                )
                SheetRow(stringResource(R.string.copy_id)) { copy(im.id); picked = null }
                if (im.tagsCount > 1) {
                    im.tagsList.forEach { tag ->
                        SheetRow(stringResource(R.string.delete_tag, tag), enabled = !im.blocked(), hint = lockHint.takeIf { im.blocked() }) {
                            picked = null
                            confirm = context.getString(R.string.delete_tag_confirm, tag) to { remove(listOf(NodesRepo.tag(im.id, tag)), true) }
                        }
                    }
                }
                SheetRow(stringResource(R.string.delete_image), NagiTheme.colors.destructive, enabled = !im.blocked(), hint = lockHint.takeIf { im.blocked() }) {
                    picked = null
                    confirm = deleteConfirm to { remove(listOf(NodesRepo.wholeImage(im.id, im.tagsList)), false) }
                }
            }
        }
    }

    confirm?.let { (text, run) ->
        NagiDialog(
            title = stringResource(R.string.delete), text = text, confirm = stringResource(R.string.delete), cancel = stringResource(R.string.cancel),
            danger = true, onDismiss = { confirm = null }, onConfirm = { confirm = null; run() },
        )
    }

    pruning?.let { d ->
        PruneDialog(repo, nodeId, d, onDone = { lines ->
            pruning = null
            errors = lines
            reload()
        }, onMessage = list::message) { pruning = null }
    }

    if (pulling) {
        PullSheet(repo, nodeId, nodeName, onPulled = reload) { pulling = false }
    }
}

/** 清理的确认：列出节点算出的可清理镜像和能释放的空间，确认后只删这些（节点会再核对一次）。 */
@Composable
private fun PruneDialog(repo: NodesRepo, nodeId: String, d: NodeImages, onDone: (List<String>) -> Unit, onMessage: (String) -> Unit, onDismiss: () -> Unit) {
    val candidates = d.imagesList.filter { it.id in d.pruneIdsList }
    val cache = d.buildCacheBytes
    var withCache by remember { mutableStateOf(false) }
    val action = rememberAction()
    val nothing = stringResource(R.string.prune_nothing)
    if (candidates.isEmpty() && cache <= 0) {
        LaunchedEffect(Unit) {
            onMessage(nothing)
            onDismiss()
        }
        return
    }
    val previous = candidates.count { it.previousVersion }
    val local = candidates.count { it.localBuild }
    val context = LocalContext.current
    val untagged = stringResource(R.string.untagged)
    NagiDialog(
        title = stringResource(R.string.prune_title), text = stringResource(R.string.prune_description),
        confirm = stringResource(R.string.prune), cancel = stringResource(R.string.cancel), danger = true,
        busy = action.busy != null, confirmEnabled = candidates.isNotEmpty() || withCache,
        onDismiss = onDismiss,
        onConfirm = {
            action.run("prune") {
                val res = repo.pruneImages(nodeId, d.pruneIdsList, withCache && cache > 0)
                onMessage(if (withCache && cache > 0) context.getString(R.string.prune_done, res.deleted, formatBytesText(res.buildCacheFreed.toDouble())) else context.getString(R.string.images_deleted, res.deleted))
                val lines = res.failedList.map { f -> if (f.id.isNotEmpty()) "${shortId(f.id)}：${f.error}" else f.error }
                onDone(if (res.skipped > 0) listOf(context.getString(R.string.prune_skipped, res.skipped)) + lines else lines)
            }
        },
    ) {
        if (candidates.isNotEmpty()) {
            Spacer(Modifier.height(12.dp))
            Text(
                stringResource(R.string.prune_images, candidates.size) + " · " + stringResource(R.string.prune_freed, formatBytesText(d.pruneBytes.toDouble())),
                style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground,
            )
            Spacer(Modifier.height(4.dp))
            val limit = 6
            candidates.take(limit).forEach { im ->
                Row(Modifier.fillMaxWidth().padding(vertical = 2.dp)) {
                    val marks = listOfNotNull(
                        stringResource(R.string.prune_previous).takeIf { im.previousVersion },
                        stringResource(R.string.prune_local).takeIf { im.localBuild },
                    )
                    Text(
                        (im.tagsList.joinToString(", ").ifEmpty { "$untagged · ${shortId(im.id)}" }) + marks.joinToString("") { " · $it" },
                        style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f),
                    )
                    Spacer(Modifier.width(12.dp))
                    Text(formatBytesText(im.size.toDouble()), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
                }
            }
            if (candidates.size > limit) {
                Text(stringResource(R.string.and_more, candidates.size - limit), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            }
            if (previous > 0 || local > 0) {
                Spacer(Modifier.height(8.dp))
                val parts = listOfNotNull(
                    stringResource(R.string.prune_warn_previous, previous).takeIf { previous > 0 },
                    stringResource(R.string.prune_warn_local, local).takeIf { local > 0 },
                )
                Text(parts.joinToString(stringResource(R.string.list_separator)), style = NagiTheme.type.secondary, color = NagiTheme.colors.warningText)
            }
        }
        if (cache > 0) {
            Spacer(Modifier.height(8.dp))
            SwitchRow(
                stringResource(R.string.prune_build_cache, formatBytesText(cache.toDouble())), withCache, { withCache = !withCache },
                SwitchLabels(stringResource(R.string.on), stringResource(R.string.off), stringResource(R.string.turn_on), stringResource(R.string.turn_off)),
            )
        }
        ActionError(action)
    }
}

/** 拉取镜像：拉取中显示进度，「取消拉取」或关掉时断开请求，节点随之停止拉取。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun PullSheet(repo: NodesRepo, nodeId: String, nodeName: String, onPulled: () -> Unit, onClose: () -> Unit) {
    val scope = rememberCoroutineScope()
    var image by remember { mutableStateOf("") }
    var fieldError by remember { mutableStateOf<String?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var progress by remember { mutableStateOf<ImagePullEvent?>(null) }
    var pulled by remember { mutableStateOf("") }
    var job by remember { mutableStateOf<Job?>(null) }
    val required = stringResource(R.string.image_required)
    val unreachable = stringResource(R.string.not_done_unreachable)
    // 关掉（离开）时断开还在进行的拉取。
    DisposableEffect(Unit) { onDispose { job?.cancel() } }
    val start = {
        val ref = image.trim()
        if (ref.isEmpty()) {
            fieldError = required
        } else if (job == null) {
            fieldError = null
            error = null
            progress = null
            pulled = ""
            job = scope.launch {
                try {
                    repo.pullImage(nodeId, ref) { ev ->
                        progress = ev
                        if (ev.done) {
                            pulled = ref
                            onPulled()
                        }
                    }
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    val pe: PanelException = e.toPanelException()
                    // 镜像格式不对时写在输入框下面。
                    if (pe.code == Code.INVALID_ARGUMENT) fieldError = pe.message
                    else error = if (pe is UnreachableException) unreachable else pe.message
                } finally {
                    job = null
                }
            }
        }
    }
    val state = when {
        pulled.isNotEmpty() -> 2
        job != null -> 1
        else -> 0
    }
    ModalBottomSheet(onDismissRequest = onClose, containerColor = NagiTheme.colors.card, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 16.dp)) {
            Text(stringResource(R.string.pull_image), style = NagiTheme.type.section, color = NagiTheme.colors.foreground)
            if (nodeName.isNotEmpty()) {
                Text(stringResource(R.string.pull_description, nodeName), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            }
            Spacer(Modifier.height(16.dp))
            Text(stringResource(R.string.image), style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground)
            Spacer(Modifier.height(8.dp))
            NagiTextField(
                image, { image = it; fieldError = null }, label = stringResource(R.string.image), placeholder = "nginx:1.27",
                error = fieldError != null, enabled = state == 0, keyboardType = KeyboardType.Uri, onDone = { start() },
            )
            Spacer(Modifier.height(6.dp))
            Text(fieldError ?: stringResource(R.string.image_help), style = NagiTheme.type.secondary, color = if (fieldError != null) NagiTheme.colors.destructive else NagiTheme.colors.mutedForeground)
            if (state != 0) {
                val p = progress
                val total = p?.total ?: 0L
                val current = p?.current ?: 0L
                val fraction = if (state == 2) 1f else if (total > 0) (current.toFloat() / total).coerceIn(0f, 1f) else 0f
                Spacer(Modifier.height(16.dp))
                UsageBar(fraction, NagiTheme.colors.primary, height = 4.dp)
                Spacer(Modifier.height(6.dp))
                Row {
                    Text(
                        if (state == 2) stringResource(R.string.pulled, pulled) else p?.status?.ifEmpty { null } ?: stringResource(R.string.pull_starting),
                        style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f),
                    )
                    if (p != null && p.layers > 0 && state != 2) {
                        Spacer(Modifier.width(12.dp))
                        val parts = listOfNotNull(
                            if (total > 0) "${formatBytesText(current.toDouble())} / ${formatBytesText(total.toDouble())}" else null,
                            stringResource(R.string.layers, p.layersDone, p.layers),
                        )
                        Text(parts.joinToString(" · "), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, maxLines = 1)
                    }
                }
            }
            error?.let {
                Text(it, style = NagiTheme.type.body, color = NagiTheme.colors.destructive, modifier = Modifier.padding(top = 8.dp))
            }
            Spacer(Modifier.height(20.dp))
            when (state) {
                1 -> NagiButton(stringResource(R.string.cancel_pull), { job?.cancel() }, Modifier.fillMaxWidth(), variant = ButtonVariant.Outline)
                2 -> NagiButton(stringResource(R.string.done), onClose, Modifier.fillMaxWidth())
                else -> Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    NagiButton(stringResource(R.string.cancel), onClose, Modifier.weight(1f), variant = ButtonVariant.Outline)
                    NagiButton(stringResource(R.string.pull), { start() }, Modifier.weight(1f))
                }
            }
        }
    }
}
