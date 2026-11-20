package ink.sipc.nagi.nodes

import android.content.ContentValues
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.provider.MediaStore
import android.provider.OpenableColumns
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.horizontalScroll
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
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CheckboxDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.viewmodel.compose.viewModel
import ink.sipc.nagi.data.NODE_EDIT_MAX
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.data.isArchive
import ink.sipc.nagi.data.joinPath
import ink.sipc.nagi.data.parentPath
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.proto.v1.NodeFileEntry
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.LocalElevate
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiIconButton
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.UsageBar
import ink.sipc.nagi.ui.formatBytesText
import ink.sipc.nagi.ui.rowClickable
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.launch
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

// 节点的文件（docs/node-access.md#文件、docs/android.md#文件）：和网页的节点文件管理一样。顶部是路径，一行是图标、名称、
// 第二行的修改时间、右边的大小和 ⋯；长按一项进入多选；复制、剪切后上面一行写「已复制 N 项」和「粘贴到此处」。
// 进入时先验证一次身份（系统验证框）。

/** 底部弹出面板、对话框里要做的事。 */
private sealed interface FileDialog {
    data class Entry(val e: NodeFileEntry) : FileDialog
    data object Dir : FileDialog
    data class Name(val kind: String, val initial: String, val target: NodeFileEntry? = null) : FileDialog
    data class Delete(val entries: List<NodeFileEntry>) : FileDialog
    data class Conflict(val names: List<String>, val answer: CompletableDeferred<String?>) : FileDialog
    data class Confirm(val text: String, val answer: CompletableDeferred<Boolean>) : FileDialog
    data class Compress(val names: List<String>) : FileDialog
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun FilesTab(session: PanelSession, n: Node, onTerminal: (String) -> Unit, showMessage: (String) -> Unit, modifier: Modifier = Modifier) {
    val vm: NodeFilesViewModel = viewModel(key = "files-${n.id}", factory = Factory { NodeFilesViewModel(session, n.id) })
    vm.elevate = LocalElevate.current
    vm.onMessage = showMessage
    LaunchedEffect(Unit) { if (vm.entries == null) vm.load() }
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val c = NagiTheme.colors
    var dialog by remember { mutableStateOf<FileDialog?>(null) }
    var editing by remember { mutableStateOf<String?>(null) }
    val unreachable = stringResource(R.string.not_done_unreachable)
    val chunked = n.info.fileOps

    suspend fun askConflict(names: List<String>): String? {
        val d = CompletableDeferred<String?>()
        dialog = FileDialog.Conflict(names, d)
        return d.await()
    }

    suspend fun confirm(text: String): Boolean {
        val d = CompletableDeferred<Boolean>()
        dialog = FileDialog.Confirm(text, d)
        return d.await()
    }

    val sensitiveOp = { p: String -> context.getString(R.string.files_sensitive_op, p) }

    // 上传：从系统的文件选择器选，可以多选。
    val pick = rememberLauncherForActivityResult(ActivityResultContracts.OpenMultipleDocuments()) { uris ->
        if (uris.isEmpty()) return@rememberLauncherForActivityResult
        val picked = uris.mapNotNull { u -> nameAndSize(context, u)?.let { Triple(u, it.first, it.second) } }
        val existing = vm.entries.orEmpty().associateBy { it.name }
        scope.launch {
            val clash = picked.filter { existing[it.second] != null }
            if (clash.isNotEmpty() || vm.access == "confirm") {
                val large = clash.any { (existing[it.second]?.size ?: 0) > NODE_EDIT_MAX }
                val names = (if (clash.isNotEmpty()) clash else picked).joinToString(context.getString(R.string.list_separator)) { it.second }
                if (!confirm(context.getString(if (large) R.string.files_overwrite_large else R.string.files_overwrite_confirm, names))) return@launch
            }
            val dir = vm.real
            val sensitive = vm.access == "confirm"
            vm.transfer(context.getString(R.string.files_uploading), unreachable) { progress ->
                val total = picked.sumOf { it.third }
                var before = 0L
                for ((uri, name, size) in picked) {
                    vm.files.upload(joinPath(dir, name), size, chunked, sensitive || existing[name]?.access == "confirm", open = {
                        context.contentResolver.openInputStream(uri) ?: error(name)
                    }) { done -> progress(before + done, total) }
                    before += size
                }
                vm.message(context.getString(R.string.files_uploaded, picked.size))
            }
        }
    }

    // 下载：存到系统的「下载」目录（Android 10 起用 MediaStore；Android 9 让用户选位置）。
    var pendingDownload by remember { mutableStateOf<Pair<String, String>?>(null) }
    val saveAs = rememberLauncherForActivityResult(ActivityResultContracts.CreateDocument("application/octet-stream")) { uri ->
        val (path, _) = pendingDownload ?: return@rememberLauncherForActivityResult
        pendingDownload = null
        if (uri != null) download(vm, context, path, uri, unreachable)
    }
    val downloadOf = { path: String, isDir: Boolean ->
        val name = path.substringAfterLast('/').ifEmpty { "root" } + if (isDir) ".tar.gz" else ""
        if (Build.VERSION.SDK_INT >= 29) {
            val values = ContentValues().apply {
                put(MediaStore.Downloads.DISPLAY_NAME, name)
                put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS)
                put(MediaStore.Downloads.IS_PENDING, 1)
            }
            val uri = context.contentResolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)
            if (uri != null) download(vm, context, path, uri, unreachable, pending = true)
        } else {
            pendingDownload = path to name
            saveAs.launch(name)
        }
    }

    val copied = vm.clip
    Column(modifier.fillMaxSize()) {
        // 路径：每一级都能点；右边是这个目录的操作（上传、新建、在此打开终端……）。
        Row(Modifier.fillMaxWidth().padding(start = Dimens.pageHorizontal, end = 8.dp).heightIn(min = 48.dp), verticalAlignment = Alignment.CenterVertically) {
            Row(Modifier.weight(1f).horizontalScroll(rememberScrollState()), verticalAlignment = Alignment.CenterVertically) {
                val parts = vm.real.split('/').filter { it.isNotEmpty() }
                Crumb(stringResource(R.string.files_root), parts.isEmpty()) { vm.open("/") }
                parts.forEachIndexed { i, p ->
                    Text(" / ", style = NagiTheme.type.mono, color = c.mutedForeground)
                    Crumb(p, i == parts.lastIndex) { vm.open("/" + parts.take(i + 1).joinToString("/")) }
                }
            }
            NagiIconButton(NagiIcons.More, stringResource(R.string.more), { dialog = FileDialog.Dir })
        }
        when (vm.access) {
            "readonly" -> Note(stringResource(R.string.files_readonly_dir))
            "confirm" -> Note(stringResource(R.string.files_sensitive_dir))
        }
        vm.progress?.let { p ->
            Column(Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 6.dp)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    val detail = listOfNotNull(
                        p.files.takeIf { it > 0 }?.let { stringResource(R.string.files_count, it.toInt()) },
                        p.done.takeIf { it > 0 }?.let { if (p.total > 0) "${formatBytesText(it.toDouble())} / ${formatBytesText(p.total.toDouble())}" else formatBytesText(it.toDouble()) },
                    ).joinToString(" · ")
                    Text(listOf(p.label, detail).filter { it.isNotEmpty() }.joinToString(" · "), style = NagiTheme.type.secondary, color = c.mutedForeground, modifier = Modifier.weight(1f), maxLines = 1, overflow = TextOverflow.Ellipsis)
                    NagiButton(stringResource(R.string.cancel), vm::cancelOp, variant = ButtonVariant.Ghost, size = ButtonSize.Small)
                }
                UsageBar(if (p.total > 0) (p.done.toFloat() / p.total) else 0f, c.primary)
            }
        }
        if (copied != null && !vm.selecting) {
            Row(Modifier.padding(horizontal = Dimens.pageHorizontal).heightIn(min = 44.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(
                    stringResource(if (copied.cut) R.string.files_cut_n else R.string.files_copied_n, copied.names.size),
                    style = NagiTheme.type.secondary, color = c.mutedForeground, modifier = Modifier.weight(1f),
                )
                NagiButton(stringResource(R.string.files_paste_here), {
                    val paths = copied.names.map { joinPath(copied.dir, it) }
                    val dest = vm.real
                    vm.op(context.getString(if (copied.cut) R.string.files_moving else R.string.files_copying), unreachable, ::askConflict, { conflict, tick ->
                        vm.sensitive(vm.access == "confirm", { confirm(sensitiveOp(dest)) }) { ok ->
                            vm.files.transfer(paths, dest, !copied.cut, conflict, ok) { ev -> tick(ev.bytes, ev.files) }
                        }
                    }) { res ->
                        if (copied.cut) vm.clip = null
                        skippedMessage(vm, context, res)
                    }
                }, variant = ButtonVariant.Outline, size = ButtonSize.Small, enabled = vm.access != "readonly")
                NagiIconButton(NagiIcons.X, stringResource(R.string.cancel), { vm.clip = null })
            }
        }
        if (vm.selecting) {
            val chosen = vm.entries.orEmpty().filter { it.name in vm.selected }
            Row(Modifier.padding(start = Dimens.pageHorizontal, end = 8.dp).heightIn(min = 44.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(stringResource(R.string.files_selected_n, vm.selected.size), style = NagiTheme.type.bodyMedium, color = c.foreground, modifier = Modifier.weight(1f))
                NagiButton(stringResource(R.string.copy), { vm.clip = FileClip(vm.real, vm.selected.toList(), false); vm.selected = emptySet() }, variant = ButtonVariant.Ghost, size = ButtonSize.Small)
                NagiButton(stringResource(R.string.files_cut), { vm.clip = FileClip(vm.real, vm.selected.toList(), true); vm.selected = emptySet() }, variant = ButtonVariant.Ghost, size = ButtonSize.Small, enabled = vm.access != "readonly")
                NagiButton(stringResource(R.string.files_compress), { dialog = FileDialog.Compress(vm.selected.toList()) }, variant = ButtonVariant.Ghost, size = ButtonSize.Small)
                NagiButton(stringResource(R.string.delete), { dialog = FileDialog.Delete(chosen) }, variant = ButtonVariant.Ghost, size = ButtonSize.Small, enabled = vm.access != "readonly")
                NagiIconButton(NagiIcons.X, stringResource(R.string.cancel), { vm.selected = emptySet() })
            }
        }
        vm.error.forEach { Text(it, style = NagiTheme.type.secondary, color = c.destructive, modifier = Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 2.dp)) }
        val list = vm.entries
        when {
            vm.needVerify -> Row(Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 12.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(stringResource(R.string.files_need_verify), style = NagiTheme.type.body, color = c.mutedForeground, modifier = Modifier.weight(1f))
                NagiButton(stringResource(R.string.files_verify), { vm.load() }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
            }
            list == null && vm.loadError != null -> InlineError(vm.loadError.orEmpty(), stringResource(R.string.retry), { vm.load() }, Modifier.padding(horizontal = Dimens.pageHorizontal))
            list == null -> SkeletonRows(5, Modifier.padding(horizontal = Dimens.pageHorizontal))
            else -> LazyColumn(Modifier.weight(1f).fillMaxWidth(), contentPadding = PaddingValues(start = Dimens.pageHorizontal, end = 8.dp, bottom = 24.dp)) {
                if (vm.real != "/") item(key = "..") {
                    FileRow(null, stringResource(R.string.files_up), "", false, vm.selecting, onClick = { vm.open(parentPath(vm.real)) })
                }
                if (list.isEmpty()) item(key = "empty") {
                    Text(stringResource(R.string.files_empty), style = NagiTheme.type.body, color = c.mutedForeground, modifier = Modifier.padding(vertical = 14.dp))
                }
                items(list, key = { it.name }) { e ->
                    val denied = e.access == "denied"
                    FileRow(
                        e, e.name, detailOf(e), e.name in vm.selected, vm.selecting,
                        onClick = {
                            when {
                                denied -> {}
                                vm.selecting -> vm.toggle(e.name)
                                e.isDir -> vm.open(vm.pathOf(e.name))
                                e.size <= NODE_EDIT_MAX -> editing = vm.pathOf(e.name)
                                else -> dialog = FileDialog.Entry(e)
                            }
                        },
                        onLongClick = if (denied) null else ({ vm.toggle(e.name) }),
                        onMore = if (denied || vm.selecting) null else ({ dialog = FileDialog.Entry(e) }),
                    )
                }
                if (vm.truncated) item(key = "more") {
                    NagiButton(stringResource(R.string.files_load_more), { vm.load(more = true) }, Modifier.padding(vertical = 8.dp), variant = ButtonVariant.Outline, size = ButtonSize.Small)
                }
            }
        }
    }

    editing?.let { p ->
        FileEditor(vm, p, onClose = { editing = null; vm.load() })
    }

    when (val d = dialog) {
        null -> {}
        is FileDialog.Entry -> EntrySheet(vm, d.e, onDismiss = { dialog = null }) { action ->
            val e = d.e
            val p = vm.pathOf(e.name)
            dialog = null
            when (action) {
                "open" -> if (e.isDir) vm.open(p) else editing = p
                "download" -> downloadOf(p, e.isDir)
                "terminal" -> onTerminal(p)
                "rename" -> dialog = FileDialog.Name("rename", e.name, e)
                "copy" -> vm.clip = FileClip(vm.real, listOf(e.name), false)
                "cut" -> vm.clip = FileClip(vm.real, listOf(e.name), true)
                "compress" -> dialog = FileDialog.Compress(listOf(e.name))
                "extract", "extract_new" -> {
                    val dest = vm.real
                    vm.op(context.getString(R.string.files_extracting), unreachable, ::askConflict, { conflict, tick ->
                        vm.sensitive(vm.access == "confirm", { confirm(sensitiveOp(dest)) }) { ok ->
                            vm.files.extract(p, dest, action == "extract_new", conflict, ok) { ev -> tick(ev.bytes, ev.files) }
                        }
                    }) { res -> skippedMessage(vm, context, res) }
                }
                "delete" -> dialog = FileDialog.Delete(listOf(e))
            }
        }
        FileDialog.Dir -> DirSheet(vm, onDismiss = { dialog = null }) { action ->
            dialog = null
            when (action) {
                "upload" -> pick.launch(arrayOf("*/*"))
                "mkdir" -> dialog = FileDialog.Name("mkdir", "")
                "touch" -> dialog = FileDialog.Name("touch", "")
                "goto" -> dialog = FileDialog.Name("goto", vm.real)
                "terminal" -> onTerminal(vm.real)
                "download" -> downloadOf(vm.real, true)
                "refresh" -> vm.load()
            }
        }
        is FileDialog.Name -> NameDialog(d, onDismiss = { dialog = null }) { value ->
            dialog = null
            when (d.kind) {
                "goto" -> vm.open(value.ifEmpty { "/" }.let { if (it.startsWith("/")) it else "/$it" })
                "mkdir" -> vm.quick(unreachable) { vm.files.mkdir(vm.pathOf(value)); vm.message(context.getString(R.string.files_created)) }
                "touch" -> vm.quick(unreachable) { vm.files.write(vm.pathOf(value), ByteArray(0), create = true, confirmed = vm.access == "confirm"); vm.message(context.getString(R.string.files_created)) }
                "rename" -> {
                    val e = d.target ?: return@NameDialog
                    vm.quick(unreachable) {
                        vm.sensitive(e.access == "confirm" || vm.access == "confirm", { confirm(sensitiveOp(vm.pathOf(e.name))) }) { ok -> vm.files.rename(vm.pathOf(e.name), value, ok) }
                    }
                }
            }
        }
        is FileDialog.Compress -> CompressDialog(d.names, onDismiss = { dialog = null }) { name, format ->
            dialog = null
            val paths = d.names.map { vm.pathOf(it) }
            val dest = vm.pathOf(name)
            vm.selected = emptySet()
            vm.op(context.getString(R.string.files_compressing), unreachable, ::askConflict, { conflict, tick ->
                vm.sensitive(vm.access == "confirm", { confirm(sensitiveOp(dest)) }) { ok ->
                    vm.files.compress(paths, dest, format, conflict, ok) { ev -> tick(ev.bytes, ev.files) }
                }
            }) { res -> skippedMessage(vm, context, res) }
        }
        is FileDialog.Delete -> DeleteDialog(d.entries, onDismiss = { dialog = null }) {
            dialog = null
            vm.selected = emptySet()
            vm.quick(unreachable) {
                for (e in d.entries) {
                    val typed = (e.isDir && !e.isSymlink) || e.access == "confirm"
                    vm.files.delete(vm.pathOf(e.name), if (typed) e.name else "")
                }
                vm.message(context.getString(R.string.files_deleted))
            }
        }
        is FileDialog.Conflict -> ConflictSheet(d.names, onDismiss = { dialog = null; d.answer.complete(null) }) { choice ->
            dialog = null
            d.answer.complete(choice)
        }
        is FileDialog.Confirm -> NagiDialog(
            title = stringResource(R.string.files_continue), text = d.text, confirm = stringResource(R.string.files_continue), cancel = stringResource(R.string.cancel),
            danger = true, onDismiss = { dialog = null; d.answer.complete(false) }, onConfirm = { dialog = null; d.answer.complete(true) },
        )
    }
}

/** 第二行：修改时间（具体的日期和时间），符号链接写指向哪里，禁止访问的写原因。 */
@Composable
private fun detailOf(e: NodeFileEntry): String {
    val time = if (e.hasModified()) dateTime(e.modified.seconds) else ""
    return listOfNotNull(
        time.ifEmpty { null },
        e.linkTarget.takeIf { e.isSymlink && it.isNotEmpty() }?.let { "→ $it" },
        when (e.access) {
            "denied" -> stringResource(R.string.files_denied)
            "readonly" -> stringResource(R.string.files_readonly)
            "confirm" -> stringResource(R.string.files_sensitive)
            else -> null
        },
    ).joinToString(" · ")
}

private fun dateTime(seconds: Long): String = SimpleDateFormat("yyyy-MM-dd HH:mm", Locale.getDefault()).format(Date(seconds * 1000))

@Composable
private fun Crumb(text: String, current: Boolean, onClick: () -> Unit) {
    Text(
        text, style = NagiTheme.type.mono, color = if (current) NagiTheme.colors.foreground else NagiTheme.colors.mutedForeground, maxLines = 1,
        modifier = Modifier.rowClickable(onClick, inset = 4.dp).padding(vertical = 10.dp),
    )
}

@Composable
private fun Note(text: String) {
    Text(text, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(horizontal = Dimens.pageHorizontal).padding(bottom = 6.dp))
}

/** 一行文件：图标（多选时是复选框）、名称和第二行，右边是大小和 ⋯。 */
@Composable
private fun FileRow(
    e: NodeFileEntry?,
    name: String,
    detail: String,
    checked: Boolean,
    selecting: Boolean,
    onClick: () -> Unit,
    onLongClick: (() -> Unit)? = null,
    onMore: (() -> Unit)? = null,
) {
    val c = NagiTheme.colors
    val denied = e?.access == "denied"
    Row(
        Modifier.fillMaxWidth().rowClickable(onClick, onLongClick = onLongClick).heightIn(min = Dimens.rowHeightTwoLine),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (selecting && e != null) {
            Checkbox(
                checked, { onClick() }, Modifier.size(20.dp),
                colors = CheckboxDefaults.colors(checkedColor = c.primary, checkmarkColor = c.primaryForeground, uncheckedColor = c.mutedForeground),
            )
        } else {
            Icon(if (e == null || e.isDir) NagiIcons.Folder else NagiIcons.File, contentDescription = null, tint = c.mutedForeground, modifier = Modifier.size(18.dp))
        }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f).padding(vertical = 6.dp)) {
            Text(name, style = NagiTheme.type.body, color = if (denied) c.mutedForeground else c.foreground, maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (detail.isNotEmpty()) Text(detail, style = NagiTheme.type.secondary, color = c.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        if (e != null && !e.isDir) {
            Spacer(Modifier.width(12.dp))
            Text(formatBytesText(e.size.toDouble()), style = NagiTheme.type.secondary, color = c.mutedForeground)
        }
        if (onMore != null) NagiIconButton(NagiIcons.More, stringResource(R.string.more), onMore) else Spacer(Modifier.width(12.dp))
    }
}

/** 一项的操作（docs/android.md「文件」：下载、编辑、重命名、复制、剪切、压缩、解压、删除）。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun EntrySheet(vm: NodeFilesViewModel, e: NodeFileEntry, onDismiss: () -> Unit, onAction: (String) -> Unit) {
    val readonly = vm.access == "readonly" || e.access == "readonly"
    ModalBottomSheet(onDismissRequest = onDismiss, containerColor = NagiTheme.colors.card) {
        Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 12.dp)) {
            Text(e.name, style = NagiTheme.type.section, color = NagiTheme.colors.foreground, maxLines = 2, overflow = TextOverflow.Ellipsis)
            Text(detailOf(e), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(bottom = 8.dp))
            if (e.isDir) {
                SheetRow(stringResource(R.string.files_open)) { onAction("open") }
                SheetRow(stringResource(R.string.files_open_terminal)) { onAction("terminal") }
            } else if (e.size <= NODE_EDIT_MAX) {
                SheetRow(stringResource(if (readonly) R.string.files_view else R.string.files_edit)) { onAction("open") }
            }
            SheetRow(stringResource(R.string.files_download)) { onAction("download") }
            SheetRow(stringResource(R.string.rename), enabled = !readonly) { onAction("rename") }
            SheetRow(stringResource(R.string.copy)) { onAction("copy") }
            SheetRow(stringResource(R.string.files_cut), enabled = !readonly) { onAction("cut") }
            SheetRow(stringResource(R.string.files_compress)) { onAction("compress") }
            if (!e.isDir && isArchive(e.name)) {
                SheetRow(stringResource(R.string.files_extract_here), enabled = !readonly) { onAction("extract") }
                SheetRow(stringResource(R.string.files_extract_new), enabled = !readonly) { onAction("extract_new") }
            }
            SheetRow(stringResource(R.string.delete), NagiTheme.colors.destructive, enabled = !readonly) { onAction("delete") }
        }
    }
}

/** 当前目录的操作。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DirSheet(vm: NodeFilesViewModel, onDismiss: () -> Unit, onAction: (String) -> Unit) {
    val readonly = vm.access == "readonly"
    ModalBottomSheet(onDismissRequest = onDismiss, containerColor = NagiTheme.colors.card) {
        Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 12.dp)) {
            Text(vm.real, style = NagiTheme.type.mono, color = NagiTheme.colors.foreground, modifier = Modifier.padding(bottom = 8.dp))
            SheetRow(stringResource(R.string.files_upload), enabled = !readonly) { onAction("upload") }
            SheetRow(stringResource(R.string.files_new_dir), enabled = !readonly) { onAction("mkdir") }
            SheetRow(stringResource(R.string.files_new_file), enabled = !readonly) { onAction("touch") }
            SheetRow(stringResource(R.string.files_goto)) { onAction("goto") }
            SheetRow(stringResource(R.string.files_open_terminal)) { onAction("terminal") }
            if (vm.real != "/") SheetRow(stringResource(R.string.files_download_dir)) { onAction("download") }
            SheetRow(stringResource(R.string.files_refresh)) { onAction("refresh") }
            Text(stringResource(R.string.files_rules), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(top = 8.dp))
        }
    }
}

/** 输入一个名称或路径（新建目录、新建文件、重命名、转到路径）。 */
@Composable
private fun NameDialog(d: FileDialog.Name, onDismiss: () -> Unit, onDone: (String) -> Unit) {
    var value by remember { mutableStateOf(d.initial) }
    val title = stringResource(
        when (d.kind) {
            "mkdir" -> R.string.files_new_dir
            "touch" -> R.string.files_new_file
            "goto" -> R.string.files_goto
            else -> R.string.rename
        },
    )
    val ok = value.isNotBlank() && (d.kind == "goto" || ('/' !in value && value != "." && value != ".."))
    NagiDialog(
        title = title, confirm = stringResource(if (d.kind == "goto") R.string.files_goto_submit else R.string.save), cancel = stringResource(R.string.cancel),
        onDismiss = onDismiss, onConfirm = { if (ok) onDone(value.trim()) }, confirmEnabled = ok,
    ) {
        Spacer(Modifier.height(16.dp))
        NagiTextField(
            value, { value = it },
            label = stringResource(if (d.kind == "goto") R.string.files_path else if (d.kind == "mkdir") R.string.files_dir_name else R.string.files_file_name),
            // 路径和文件名多是英文：用网址类型的键盘，多数输入法会默认英文、不联想。
            keyboardType = androidx.compose.ui.text.input.KeyboardType.Uri,
            onDone = { if (ok) onDone(value.trim()) },
        )
    }
}

/** 压缩：名称和格式（zip、tar.gz）。 */
@Composable
private fun CompressDialog(names: List<String>, onDismiss: () -> Unit, onDone: (String, String) -> Unit) {
    var format by remember { mutableStateOf("zip") }
    val base = if (names.size == 1) names[0] else "archive"
    var name by remember { mutableStateOf(base) }
    val ext = if (format == "zip") ".zip" else ".tar.gz"
    val ok = name.isNotBlank() && '/' !in name
    NagiDialog(
        title = stringResource(R.string.files_compress), text = stringResource(R.string.files_compress_n, names.size),
        confirm = stringResource(R.string.files_compress), cancel = stringResource(R.string.cancel),
        onDismiss = onDismiss, onConfirm = { onDone(name.trim() + ext, format) }, confirmEnabled = ok,
    ) {
        Spacer(Modifier.height(16.dp))
        NagiTextField(name, { name = it }, label = stringResource(R.string.files_file_name))
        Spacer(Modifier.height(8.dp))
        ink.sipc.nagi.ui.UnderlineTabs(listOf(".zip", ".tar.gz"), if (format == "zip") 0 else 1, { format = if (it == 0) "zip" else "tar.gz" }, spacing = 20.dp)
    }
}

/** 删除：目录和敏感文件要输入名称（和网页一样）。 */
@Composable
private fun DeleteDialog(entries: List<NodeFileEntry>, onDismiss: () -> Unit, onConfirm: () -> Unit) {
    val one = entries.singleOrNull()
    val typed = one != null && ((one.isDir && !one.isSymlink) || one.access == "confirm")
    var value by remember { mutableStateOf("") }
    val text = if (one != null) stringResource(if (one.isDir && !one.isSymlink) R.string.files_delete_dir_confirm else R.string.files_delete_confirm, one.name)
    else stringResource(R.string.files_delete_many_confirm, entries.size)
    // 多选里有目录或敏感文件时也要逐个确认：不一起删，让用户一个一个来。
    val blocked = one == null && entries.any { (it.isDir && !it.isSymlink) || it.access == "confirm" }
    NagiDialog(
        title = stringResource(R.string.delete), text = if (blocked) stringResource(R.string.files_delete_one_by_one) else text,
        confirm = stringResource(R.string.delete), cancel = stringResource(R.string.cancel), danger = true,
        onDismiss = onDismiss, onConfirm = onConfirm, confirmEnabled = !blocked && (!typed || value == one?.name),
    ) {
        if (typed) {
            Spacer(Modifier.height(16.dp))
            NagiTextField(value, { value = it }, label = stringResource(R.string.files_type_name, one?.name.orEmpty()), placeholder = one?.name)
        }
    }
}

/** 同名的处理：覆盖、保留两者、跳过。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ConflictSheet(names: List<String>, onDismiss: () -> Unit, onChoose: (String) -> Unit) {
    ModalBottomSheet(onDismissRequest = onDismiss, containerColor = NagiTheme.colors.card) {
        Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 12.dp)) {
            Text(stringResource(R.string.files_conflict_title, names.joinToString(stringResource(R.string.list_separator))), style = NagiTheme.type.section, color = NagiTheme.colors.foreground, maxLines = 3, overflow = TextOverflow.Ellipsis)
            Text(stringResource(R.string.files_overwrite_hint), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(vertical = 8.dp))
            SheetRow(stringResource(R.string.files_keep_both)) { onChoose("rename") }
            SheetRow(stringResource(R.string.files_skip)) { onChoose("skip") }
            SheetRow(stringResource(R.string.files_overwrite), NagiTheme.colors.destructive) { onChoose("overwrite") }
        }
    }
}

private fun skippedMessage(vm: NodeFilesViewModel, context: android.content.Context, res: ink.sipc.nagi.proto.v1.FileOpResult) {
    if (res.skippedLinks > 0) vm.message(context.getString(R.string.files_skipped_links, res.skippedLinks))
    if (res.skipped > 0) vm.message(context.getString(R.string.files_skipped, res.skipped))
}

/** 选中的文件的名称和大小。 */
private fun nameAndSize(context: android.content.Context, uri: Uri): Pair<String, Long>? =
    context.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE), null, null, null)?.use { cur ->
        if (!cur.moveToFirst()) return null
        val name = cur.getString(0) ?: return null
        val size = if (cur.isNull(1)) -1L else cur.getLong(1)
        name to size
    }

/** 下载到 [uri]（MediaStore 的条目下载完再显示出来）。 */
private fun download(vm: NodeFilesViewModel, context: android.content.Context, path: String, uri: Uri, unreachable: String, pending: Boolean = false) {
    vm.transfer(context.getString(R.string.files_downloading), unreachable) { progress ->
        try {
            val out = context.contentResolver.openOutputStream(uri) ?: error(path)
            out.use { vm.files.download(path, it, progress) }
            if (pending && Build.VERSION.SDK_INT >= 29) {
                context.contentResolver.update(uri, ContentValues().apply { put(MediaStore.Downloads.IS_PENDING, 0) }, null, null)
            }
            vm.message(context.getString(R.string.files_downloaded))
        } catch (e: Throwable) {
            // 没下完的不留在「下载」里。
            runCatching { context.contentResolver.delete(uri, null, null) }
            throw e
        }
    }
}
