package ink.sipc.nagi.nodes

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material3.ExperimentalMaterial3Api
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
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import com.connectrpc.Code
import ink.sipc.nagi.network.ElevationRequiredException
import ink.sipc.nagi.network.PanelException
import ink.sipc.nagi.proto.v1.NodeFileBackup
import ink.sipc.nagi.proto.v1.ReadNodeFileResponse
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.CodeEditorPane
import ink.sipc.nagi.ui.CodeEditorState
import ink.sipc.nagi.ui.DiffLine
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.EditorKeyLabels
import ink.sipc.nagi.ui.EditorKeys
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SkeletonRows
import ink.sipc.nagi.ui.editorLanguageFor
import ink.sipc.nagi.ui.formatBytesText
import ink.sipc.nagi.ui.lineDiff
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import java.security.MessageDigest
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

// 编辑节点上的文件（docs/node-access.md#文件、docs/android.md「编辑器」）：整页，1 MiB 以内的文本。保存前显示和打开时的差异；
// 打开以后文件在节点上被改过时（SHA-256 不同）面板拒绝，显示差异，选覆盖或用节点上的内容。敏感文件保存前再确认一次。
// 覆盖前节点保留旧内容，可以在「历史版本」里恢复。有没保存的修改时，返回前问放弃还是继续编辑。

private fun sha256(b: ByteArray) = MessageDigest.getInstance("SHA-256").digest(b).joinToString("") { "%02x".format(it) }

@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun FileEditor(vm: NodeFilesViewModel, path: String, onClose: () -> Unit) {
    val scope = rememberCoroutineScope()
    val context = LocalContext.current
    val c = NagiTheme.colors
    var file by remember { mutableStateOf<ReadNodeFileResponse?>(null) }
    var original by remember { mutableStateOf("") }
    var loadError by remember { mutableStateOf<String?>(null) }
    var loads by remember { mutableStateOf(0) }
    var dirty by remember { mutableStateOf(false) }
    // 保存前的确认：差异（diff）、节点上的内容已经变了（conflict）、放弃修改（discard）、历史版本（history）。
    var sheet by remember { mutableStateOf<String?>(null) }
    var nodeText by remember { mutableStateOf("") }
    var nodeSha by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var backups by remember { mutableStateOf<List<NodeFileBackup>?>(null) }
    var restoring by remember { mutableStateOf<NodeFileBackup?>(null) }
    val editor = remember(loads) { CodeEditorState() }
    val name = path.substringAfterLast('/')

    LaunchedEffect(loads) {
        loadError = null
        try {
            val f = vm.elevated { vm.files.read(path) }
            file = f
            original = f.content.toStringUtf8()
            dirty = false
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            loadError = e.message.orEmpty()
        }
    }

    val readonly = file?.let { it.access == "readonly" || it.binary } ?: true
    val sensitive = file?.access == "confirm"

    fun save(expected: String, confirmed: Boolean) {
        busy = true
        error = null
        scope.launch {
            try {
                val res = vm.elevated { vm.files.write(path, editor.text.toByteArray(), expected, confirmed = confirmed || sensitive) }
                vm.message(context.getString(if (res.inPlace) R.string.files_saved_in_place else R.string.files_saved))
                original = editor.text
                file = file?.toBuilder()?.setSha256(res.sha256)?.build()
                dirty = false
                sheet = null
            } catch (e: CancellationException) {
                throw e
            } catch (e: PanelException) {
                if (e.code == Code.ABORTED && e !is ElevationRequiredException) {
                    // 文件在打开以后被改过：读出节点上现在的内容，让用户决定。
                    runCatching { vm.files.read(path) }.getOrNull()?.let {
                        nodeText = it.content.toStringUtf8()
                        nodeSha = it.sha256
                    }
                    sheet = "conflict"
                } else {
                    error = e.message
                }
            } catch (e: Exception) {
                error = e.message
            } finally {
                busy = false
            }
        }
    }

    fun close() {
        if (dirty) sheet = "discard" else onClose()
    }

    Dialog(onDismissRequest = ::close, properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false, dismissOnClickOutside = false)) {
        BackHandler(onBack = ::close)
        Column(Modifier.fillMaxSize().background(c.background).statusBarsPadding().navigationBarsPadding().imePadding()) {
            NagiBackTopBar(name, onBack = ::close) {
                if (file != null && !readonly) {
                    NagiButton(stringResource(R.string.files_history), {
                        sheet = "history"
                        backups = null
                        scope.launch { backups = runCatching { vm.files.backups(path) }.getOrElse { emptyList() } }
                    }, variant = ButtonVariant.Ghost, size = ButtonSize.Small)
                    Spacer(Modifier.width(4.dp))
                    NagiButton(stringResource(R.string.save), { sheet = "diff" }, enabled = dirty && !busy, size = ButtonSize.Small, loading = busy)
                    Spacer(Modifier.width(12.dp))
                }
            }
            val f = file
            Text(
                listOfNotNull(
                    path,
                    f?.let { formatBytesText(it.size.toDouble()) },
                    when {
                        f == null -> null
                        f.binary -> stringResource(R.string.files_binary)
                        f.access == "readonly" -> stringResource(R.string.files_readonly_dir)
                        f.access == "confirm" -> stringResource(R.string.files_sensitive_file)
                        else -> null
                    },
                ).joinToString(" · "),
                style = NagiTheme.type.secondary, color = if (sensitive) c.warningText else c.mutedForeground, maxLines = 2, overflow = TextOverflow.Ellipsis,
                modifier = Modifier.padding(horizontal = Dimens.pageHorizontal).padding(bottom = 6.dp),
            )
            error?.let { Text(it, style = NagiTheme.type.secondary, color = c.destructive, modifier = Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 4.dp)) }
            when {
                f == null && loadError != null -> InlineError(loadError.orEmpty(), stringResource(R.string.retry), { loads++ }, Modifier.padding(horizontal = Dimens.pageHorizontal))
                f == null -> SkeletonRows(6, Modifier.padding(horizontal = Dimens.pageHorizontal))
                else -> {
                    Box(Modifier.weight(1f).fillMaxWidth()) {
                        CodeEditorPane(
                            editor, if (f.binary) "" else original, editorLanguageFor(name), Modifier.fillMaxSize(), readOnly = readonly,
                            onChange = { dirty = editor.text != original },
                        )
                    }
                    if (!readonly) EditorKeys(editor, EditorKeyLabels(stringResource(R.string.files_undo), stringResource(R.string.files_redo)), Modifier.background(c.card))
                }
            }
        }

        when (sheet) {
            "diff" -> DiffSheet(
                title = stringResource(R.string.files_review),
                note = if (sensitive) stringResource(R.string.files_sensitive_confirm, path) else null,
                lines = remember(original, sheet) { lineDiff(original, editor.text) },
                confirm = stringResource(R.string.save), danger = sensitive, busy = busy,
                onDismiss = { sheet = null },
            ) { save(file?.sha256.orEmpty(), false) }
            "conflict" -> DiffSheet(
                title = stringResource(R.string.files_conflict_changed),
                note = stringResource(R.string.files_conflict_text),
                lines = remember(nodeText, sheet) { lineDiff(nodeText, editor.text) },
                confirm = stringResource(R.string.files_overwrite), danger = true, busy = busy,
                secondary = stringResource(R.string.files_use_node), onSecondary = { sheet = null; loads++ },
                onDismiss = { sheet = null },
            ) { save(nodeSha.ifEmpty { sha256(nodeText.toByteArray()) }, true) }
            "discard" -> NagiDialog(
                title = stringResource(R.string.files_discard_title), text = stringResource(R.string.files_discard_text),
                confirm = stringResource(R.string.files_discard), cancel = stringResource(R.string.files_keep_editing), danger = true,
                onDismiss = { sheet = null }, onConfirm = { sheet = null; onClose() },
            )
            "history" -> ModalBottomSheet(onDismissRequest = { sheet = null }, containerColor = c.card) {
                Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 12.dp)) {
                    Text(stringResource(R.string.files_history), style = NagiTheme.type.section, color = c.foreground)
                    Text(stringResource(R.string.files_history_hint), style = NagiTheme.type.secondary, color = c.mutedForeground, modifier = Modifier.padding(bottom = 8.dp))
                    val list = backups
                    when {
                        list == null -> SkeletonRows(2)
                        list.isEmpty() -> Text(stringResource(R.string.files_history_empty), style = NagiTheme.type.body, color = c.mutedForeground, modifier = Modifier.padding(vertical = 12.dp))
                        else -> list.forEach { b ->
                            Row(Modifier.fillMaxWidth().heightIn(min = 48.dp), verticalAlignment = Alignment.CenterVertically) {
                                Text(timeOf(b), style = NagiTheme.type.body, color = c.foreground, modifier = Modifier.weight(1f))
                                Text(formatBytesText(b.size.toDouble()), style = NagiTheme.type.secondary, color = c.mutedForeground)
                                Spacer(Modifier.width(12.dp))
                                NagiButton(stringResource(R.string.files_restore), { restoring = b }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                            }
                        }
                    }
                }
            }
        }
        restoring?.let { b ->
            NagiDialog(
                title = stringResource(R.string.files_restore), text = stringResource(R.string.files_restore_confirm, timeOf(b)),
                confirm = stringResource(R.string.files_restore), cancel = stringResource(R.string.cancel), busy = busy,
                onDismiss = { restoring = null },
                onConfirm = {
                    busy = true
                    scope.launch {
                        try {
                            vm.elevated { vm.files.restore(path, b.id, sensitive) }
                            vm.message(context.getString(R.string.files_restored))
                            restoring = null
                            sheet = null
                            loads++
                        } catch (e: CancellationException) {
                            throw e
                        } catch (e: Exception) {
                            error = e.message
                            restoring = null
                        } finally {
                            busy = false
                        }
                    }
                },
            )
        }
    }
}

private fun timeOf(b: NodeFileBackup) = SimpleDateFormat("yyyy-MM-dd HH:mm:ss", Locale.getDefault()).format(Date(b.time.seconds * 1000))

/** 差异：红色是删掉的行，绿色是加上的行，中间省略的写「…」。下面是操作。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DiffSheet(
    title: String,
    note: String?,
    lines: List<DiffLine>,
    confirm: String,
    danger: Boolean,
    busy: Boolean,
    onDismiss: () -> Unit,
    secondary: String? = null,
    onSecondary: () -> Unit = {},
    onConfirm: () -> Unit,
) {
    val c = NagiTheme.colors
    ModalBottomSheet(onDismissRequest = { if (!busy) onDismiss() }, containerColor = c.card) {
        Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 16.dp)) {
            Text(title, style = NagiTheme.type.section, color = c.foreground)
            if (note != null) Text(note, style = NagiTheme.type.secondary, color = if (danger) c.warningText else c.mutedForeground, modifier = Modifier.padding(top = 4.dp))
            Spacer(Modifier.padding(top = 8.dp))
            if (lines.none { it.kind == '-' || it.kind == '+' }) {
                Text(stringResource(R.string.files_same), style = NagiTheme.type.body, color = c.mutedForeground, modifier = Modifier.padding(vertical = 12.dp))
            } else {
                LazyColumn(Modifier.fillMaxWidth().heightIn(max = 360.dp).background(c.background).horizontalScroll(rememberScrollState())) {
                    items(lines) { l ->
                        val (bg, fg) = when (l.kind) {
                            '-' -> c.destructive.copy(alpha = 0.12f) to c.destructive
                            '+' -> c.success.copy(alpha = 0.12f) to c.success
                            else -> c.background to c.mutedForeground
                        }
                        Text(
                            if (l.kind == '…') "…" else "${l.kind} ${l.text}", style = NagiTheme.type.mono, color = fg, softWrap = false,
                            modifier = Modifier.background(bg).padding(horizontal = 8.dp, vertical = 1.dp),
                        )
                    }
                }
            }
            Spacer(Modifier.padding(top = 16.dp))
            Row {
                NagiButton(secondary ?: stringResource(R.string.cancel), { if (secondary != null) onSecondary() else onDismiss() }, Modifier.weight(1f), variant = ButtonVariant.Outline, enabled = !busy)
                Spacer(Modifier.width(12.dp))
                NagiButton(confirm, onConfirm, Modifier.weight(1f), variant = if (danger) ButtonVariant.Destructive else ButtonVariant.Primary, loading = busy)
            }
        }
    }
}
