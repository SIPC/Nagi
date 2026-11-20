package ink.sipc.nagi.nodes

import android.content.Context
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.data.ShellConnection
import ink.sipc.nagi.data.ShellEvent
import ink.sipc.nagi.data.openNodeShell
import ink.sipc.nagi.network.ElevationRequiredException
import ink.sipc.nagi.network.UnreachableException
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.LocalElevate
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.StatusDot
import ink.sipc.nagi.ui.TerminalAnsiDark
import ink.sipc.nagi.ui.TerminalAnsiLight
import ink.sipc.nagi.ui.TerminalKeys
import ink.sipc.nagi.ui.TerminalPane
import ink.sipc.nagi.ui.TerminalScreen
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

// 节点终端（docs/node-access.md#终端、docs/android.md#日志终端和编辑器）：以 root 身份连到节点，和网页一样。
// 连接放在 ViewModel 里：切到别的标签、旋转屏幕都不断开。离开前台超过 5 分钟断开，回来时显示「重新连接」。

/** 终端的连接状态。 */
sealed interface ShellState {
    data object Idle : ShellState
    data object Connecting : ShellState
    data object Open : ShellState

    /** 结束了：[failed] 为 true 时是出错（红字）。 */
    data class Ended(val message: String, val failed: Boolean) : ShellState
}

class NodeTerminalViewModel(private val session: PanelSession, private val nodeId: String) : ViewModel() {
    var state by mutableStateOf<ShellState>(ShellState.Idle)
        private set
    /** 连上过：之后断开了也留着屏幕上的内容。还没连上过时不显示终端区域，只写状态和原因。 */
    var everOpened by mutableStateOf(false)
        private set
    private var conn: ShellConnection? = null
    private var job: Job? = null
    /** 这次连接打开的目录（文件页的「在此打开终端」）。 */
    var openedCwd by mutableStateOf("")
        private set
    private var pendingSize: Pair<Int, Int>? = null

    val screen = TerminalScreen(
        send = { bytes -> conn?.send(bytes) },
        resize = { cols, rows -> conn?.resize(cols, rows) ?: run { pendingSize = cols to rows } },
    )

    /** 离开前台：保持 5 分钟（和文档一样），超过就断开。 */
    private var away: Job? = null
    private val lifecycle = object : DefaultLifecycleObserver {
        override fun onStop(owner: LifecycleOwner) {
            if (state != ShellState.Open) return
            away = viewModelScope.launch {
                delay(KEEP_IN_BACKGROUND_MS)
                disconnect(awayText)
            }
        }

        override fun onStart(owner: LifecycleOwner) {
            away?.cancel()
        }
    }
    private var awayText = ""

    init {
        ProcessLifecycleOwner.get().lifecycle.addObserver(lifecycle)
    }

    /**
     * 连接。[elevate] 是高权限确认（系统验证框），面板要求时弹出后再连一次；[texts] 是要显示的说明。
     */
    fun connect(cwd: String, elevate: (suspend () -> Boolean)?, texts: ShellTexts) {
        if (state == ShellState.Connecting) return
        awayText = texts.away
        conn?.close()
        conn = null
        job?.cancel()
        state = ShellState.Connecting
        openedCwd = cwd
        job = viewModelScope.launch {
            val size = pendingSize ?: (screen.cols.takeIf { it > 0 } ?: 80) to (screen.rows.takeIf { it > 0 } ?: 24)
            suspend fun open() = session.openNodeShell(nodeId, cwd, size.first, size.second, onOutput = screen::output) { ev ->
                viewModelScope.launch { onEvent(ev, texts) }
            }
            state = try {
                conn = try {
                    open()
                } catch (e: ElevationRequiredException) {
                    // 打开终端前要再验证一次身份（docs/node-access.md#权限），确认过 15 分钟内不再问。
                    if (elevate != null && elevate()) open() else throw e
                }
                // 连上之前视图可能已经量好了行列：告诉面板。
                if (screen.cols > 0) conn?.resize(screen.cols, screen.rows)
                everOpened = true
                ShellState.Open
            } catch (e: CancellationException) {
                throw e
            } catch (e: ElevationRequiredException) {
                ShellState.Ended(texts.needVerify, true)
            } catch (e: UnreachableException) {
                ShellState.Ended(texts.unreachable, true)
            } catch (e: Exception) {
                ShellState.Ended(e.message.orEmpty(), true)
            }
        }
    }

    private fun onEvent(ev: ShellEvent, texts: ShellTexts) {
        val s = state
        if (s is ShellState.Ended) return
        state = when (ev) {
            is ShellEvent.Exited -> ShellState.Ended(texts.exited(ev.code), false)
            is ShellEvent.Failed -> ShellState.Ended(ev.message, true)
            is ShellEvent.Closed -> ShellState.Ended(texts.closed, false)
        }
        conn = null
    }

    fun disconnect(message: String) {
        conn?.close()
        conn = null
        job?.cancel()
        if (state == ShellState.Open || state == ShellState.Connecting) state = ShellState.Ended(message, false)
    }

    override fun onCleared() {
        ProcessLifecycleOwner.get().lifecycle.removeObserver(lifecycle)
        conn?.close()
    }

    private companion object {
        const val KEEP_IN_BACKGROUND_MS = 5 * 60_000L
    }
}

/** 终端里要显示的说明（取自字符串资源，ViewModel 里拿不到）。 */
class ShellTexts(val exited: (Int) -> String, val closed: String, val unreachable: String, val needVerify: String, val away: String)

private const val PREFS = "terminal"
private const val TEXT_SIZE = "text_size"

/**
 * 节点页的「终端」标签：上面一行是连接状态和 `root · 节点名`（防止敲错机器），右边连接、重新连接；
 * 连上以后下面是终端和键盘上方的按键行。[cwd] 是文件页「在此打开终端」传来的目录。
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun TerminalTab(session: PanelSession, n: Node, cwd: String, modifier: Modifier = Modifier) {
    val vm: NodeTerminalViewModel = viewModel(key = "shell-${n.id}", factory = Factory { NodeTerminalViewModel(session, n.id) })
    val elevate = LocalElevate.current
    val context = LocalContext.current
    val texts = ShellTexts(
        exited = { context.getString(R.string.shell_exited, it) },
        closed = stringResource(R.string.shell_closed),
        unreachable = stringResource(R.string.not_done_unreachable),
        needVerify = stringResource(R.string.files_need_verify),
        away = stringResource(R.string.shell_away),
    )
    // 配色跟随 App 的深浅色：浅色时白底深字。
    val c = NagiTheme.colors
    val dark = c.background.luminance() < 0.5f
    LaunchedEffect(dark, c.background, c.foreground) {
        vm.screen.setColors(c.foreground, c.background, if (dark) TerminalAnsiDark else TerminalAnsiLight)
    }
    // 第一次打开这个标签时直接连接（网页是点「连接」，手机上少点一次）。
    LaunchedEffect(Unit) { if (vm.state == ShellState.Idle) vm.connect(cwd, elevate, texts) }
    // 从文件页换了目录过来：重新连接进那个目录。
    LaunchedEffect(cwd) { if (cwd.isNotEmpty() && cwd != vm.openedCwd && vm.state != ShellState.Idle) vm.connect(cwd, elevate, texts) }
    val prefs = remember { context.getSharedPreferences(PREFS, Context.MODE_PRIVATE) }
    var menu by remember { mutableStateOf(false) }
    val copy = rememberCopy()

    Column(modifier.fillMaxSize().imePadding()) {
        Row(
            Modifier.fillMaxWidth().padding(horizontal = Dimens.pageHorizontal).heightIn(min = 48.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            val s = vm.state
            Row(Modifier.weight(1f), verticalAlignment = Alignment.CenterVertically) {
                when (s) {
                    ShellState.Open -> {
                        StatusDot(c.success)
                        Spacer(Modifier.width(6.dp))
                        Text(stringResource(R.string.shell_connected), style = NagiTheme.type.secondary, color = c.mutedForeground)
                    }
                    ShellState.Connecting -> Text(stringResource(R.string.shell_connecting), style = NagiTheme.type.secondary, color = c.mutedForeground)
                    ShellState.Idle -> Text(stringResource(R.string.shell_intro), style = NagiTheme.type.secondary, color = c.mutedForeground)
                    is ShellState.Ended -> Text(
                        s.message, style = NagiTheme.type.secondary, color = if (s.failed) c.destructive else c.mutedForeground,
                        maxLines = 2, overflow = TextOverflow.Ellipsis,
                    )
                }
                if (s != ShellState.Idle) {
                    Spacer(Modifier.width(12.dp))
                    Text("root · ${n.name}", style = NagiTheme.type.mono, color = c.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis)
                }
            }
            Spacer(Modifier.width(12.dp))
            NagiButton(
                stringResource(if (s == ShellState.Idle) R.string.shell_connect else R.string.shell_reconnect),
                { vm.connect(cwd.ifEmpty { vm.openedCwd }, elevate, texts) },
                variant = if (s == ShellState.Idle) ButtonVariant.Primary else ButtonVariant.Outline, size = ButtonSize.Small,
                loading = s == ShellState.Connecting,
            )
        }
        if (n.info.inContainer) {
            Text(
                stringResource(R.string.node_access_in_container), style = NagiTheme.type.secondary, color = c.warningText,
                modifier = Modifier.padding(horizontal = Dimens.pageHorizontal).padding(bottom = 8.dp),
            )
        }
        val s = vm.state
        if (!vm.everOpened && s is ShellState.Ended) {
            // 还没连上就失败了：把原因写在中间，免得只看到一块空白的终端。
            Text(
                s.message, style = NagiTheme.type.body, color = if (s.failed) c.destructive else c.mutedForeground,
                modifier = Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 16.dp),
            )
        }
        if (vm.everOpened || s == ShellState.Open) {
            Box(Modifier.weight(1f).fillMaxWidth().background(c.background)) {
                TerminalPane(
                    vm.screen, Modifier.fillMaxSize(),
                    textSizeSp = prefs.getFloat(TEXT_SIZE, 13f),
                    autoFocus = vm.state == ShellState.Open || vm.state == ShellState.Connecting,
                    onTextSize = { prefs.edit().putFloat(TEXT_SIZE, it).apply() },
                    onLongPress = { menu = true },
                )
            }
            if (vm.state == ShellState.Open) TerminalKeys(vm.screen, Modifier.background(c.card))
        }
    }

    if (menu) {
        ModalBottomSheet(onDismissRequest = { menu = false }, containerColor = c.card) {
            Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 12.dp)) {
                SheetRow(stringResource(R.string.shell_paste)) { menu = false; vm.screen.paste() }
                SheetRow(stringResource(R.string.shell_copy_all)) { menu = false; copy(vm.screen.transcript()) }
            }
        }
    }
}
