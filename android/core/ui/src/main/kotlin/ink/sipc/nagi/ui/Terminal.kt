package ink.sipc.nagi.ui

import android.annotation.SuppressLint
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.graphics.Canvas
import android.graphics.Typeface
import android.os.Handler
import android.os.Looper
import android.text.InputType
import android.util.TypedValue
import android.view.GestureDetector
import android.view.KeyEvent
import android.view.MotionEvent
import android.view.ScaleGestureDetector
import android.view.View
import android.view.inputmethod.BaseInputConnection
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.InputConnection
import android.view.inputmethod.InputMethodManager
import androidx.compose.foundation.background
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.foundation.clickable
import com.termux.terminal.KeyHandler
import com.termux.terminal.TerminalEmulator
import com.termux.terminal.TerminalOutput
import com.termux.terminal.TerminalSession
import com.termux.terminal.TerminalSessionClient
import com.termux.terminal.TextStyle
import com.termux.view.TerminalRenderer
import kotlin.math.max
import kotlin.math.roundToInt

// 终端（docs/android.md#日志终端和编辑器）：用 Termux 的 terminal-emulator 解析输出、terminal-view 的 TerminalRenderer 绘制。
// Termux 的 TerminalSession 只能接本机进程，所以连接不用它：[TerminalScreen] 拿到远端的输出喂给解析器，
// 键盘输入交给 [TerminalScreen.send]（App 发到面板）。视图可以销毁重建（切换标签），屏幕内容在 TerminalScreen 里。

/** 一个终端的屏幕。[send] 发出输入（键盘、终端自己的应答），[resize] 在行列变化时调用。在主线程上使用。 */
class TerminalScreen(private val send: (ByteArray) -> Unit, private val resize: (cols: Int, rows: Int) -> Unit) {
    internal var emulator: TerminalEmulator? = null
        private set
    internal var view: TerminalCanvasView? = null
    private val main = Handler(Looper.getMainLooper())
    private var palette: IntArray? = null

    /** 键盘上方的 Ctrl、Alt：按一下，作用于下一个键。 */
    var ctrl by mutableStateOf(false)
    var alt by mutableStateOf(false)

    val cols: Int get() = emulator?.mColumns ?: 0
    val rows: Int get() = emulator?.mRows ?: 0

    private val output = object : TerminalOutput() {
        override fun write(data: ByteArray, offset: Int, count: Int) = send(data.copyOfRange(offset, offset + count))
        override fun titleChanged(oldTitle: String?, newTitle: String?) {}
        override fun onCopyTextToClipboard(text: String?) {
            val v = view ?: return
            v.context.getSystemService(ClipboardManager::class.java)?.setPrimaryClip(ClipData.newPlainText(null, text.orEmpty()))
        }
        override fun onPasteTextFromClipboard() = paste()
        override fun onBell() {}
        // 程序重设了颜色（OSC 104）：换回 App 的配色。
        override fun onColorsChanged() = applyPalette()
    }

    /** 远端的输出，任何线程都可以调用。 */
    fun output(bytes: ByteArray) {
        main.post {
            val e = emulator ?: return@post
            e.append(bytes, bytes.size)
            view?.onOutput()
        }
    }

    /** 输入一段文字（粘贴、键盘上方的按键）。换行写成回车，和终端里按回车一样。 */
    fun sendText(text: String) {
        if (text.isEmpty()) return
        view?.scrollToBottom()
        send(text.replace("\r\n", "\r").replace('\n', '\r').toByteArray())
    }

    /** 发一个特殊键（方向键、Esc、Tab），按当前的光标模式取转义序列。 */
    fun sendKey(keyCode: Int) {
        val e = emulator ?: return
        var mod = 0
        if (ctrl) mod = mod or KeyHandler.KEYMOD_CTRL
        if (alt) mod = mod or KeyHandler.KEYMOD_ALT
        ctrl = false
        alt = false
        val code = KeyHandler.getCode(keyCode, mod, e.isCursorKeysApplicationMode, e.isKeypadApplicationMode) ?: return
        sendText(code)
    }

    fun paste() {
        val v = view ?: return
        val clip = v.context.getSystemService(ClipboardManager::class.java)?.primaryClip ?: return
        sendText(clip.getItemAt(0)?.coerceToText(v.context)?.toString().orEmpty())
    }

    /** 屏幕上和回滚里的全部文字（复制）。 */
    fun transcript(): String = emulator?.screen?.transcriptTextWithFullLinesJoined?.trimEnd().orEmpty()

    /** 配色：[foreground]、[background] 和 16 个 ANSI 颜色（浅色时换成在白底上看得清的一套）。 */
    fun setColors(foreground: Color, background: Color, ansi: List<Color>) {
        palette = IntArray(18).also { p ->
            ansi.take(16).forEachIndexed { i, c -> p[i] = c.toArgb() }
            p[16] = foreground.toArgb()
            p[17] = background.toArgb()
        }
        applyPalette()
    }

    private fun applyPalette() {
        val p = palette ?: return
        val colors = emulator?.mColors?.mCurrentColors ?: return
        for (i in 0 until 16) colors[i] = p[i]
        colors[TextStyle.COLOR_INDEX_FOREGROUND] = p[16]
        colors[TextStyle.COLOR_INDEX_BACKGROUND] = p[17]
        colors[TextStyle.COLOR_INDEX_CURSOR] = p[16]
        view?.invalidate()
    }

    internal val background: Int get() = palette?.get(17) ?: android.graphics.Color.BLACK

    /** 视图量出了能放几行几列。第一次时创建解析器（回滚保留 2000 行）。 */
    internal fun sizeTo(cols: Int, rows: Int) {
        val e = emulator
        if (e == null) {
            emulator = TerminalEmulator(output, cols, rows, 2000, NoClient)
            applyPalette()
        } else if (e.mColumns != cols || e.mRows != rows) {
            e.resize(cols, rows)
        } else {
            return
        }
        resize(cols, rows)
    }

    /** 解析器要的回调：日志都不要（远端的内容不进本机日志）。 */
    private object NoClient : TerminalSessionClient {
        override fun onTextChanged(changedSession: TerminalSession) {}
        override fun onTitleChanged(changedSession: TerminalSession) {}
        override fun onSessionFinished(finishedSession: TerminalSession) {}
        override fun onCopyTextToClipboard(session: TerminalSession, text: String?) {}
        override fun onPasteTextFromClipboard(session: TerminalSession?) {}
        override fun onBell(session: TerminalSession) {}
        override fun onColorsChanged(session: TerminalSession) {}
        override fun onTerminalCursorStateChange(state: Boolean) {}
        override fun getTerminalCursorStyle(): Int? = null
        override fun logError(tag: String?, message: String?) {}
        override fun logWarn(tag: String?, message: String?) {}
        override fun logInfo(tag: String?, message: String?) {}
        override fun logDebug(tag: String?, message: String?) {}
        override fun logVerbose(tag: String?, message: String?) {}
        override fun logStackTraceWithMessage(tag: String?, message: String?, e: Exception?) {}
        override fun logStackTrace(tag: String?, e: Exception?) {}
    }
}

/** 终端的颜色：深色时用常见的一套；浅色时白底深字，ANSI 颜色换成在白底上看得清的一套（GitHub 浅色主题的取值）。 */
val TerminalAnsiDark = listOf(
    0xFF000000, 0xFFCD3131, 0xFF0DBC79, 0xFFE5E510, 0xFF2472C8, 0xFFBC3FBC, 0xFF11A8CD, 0xFFE5E5E5,
    0xFF666666, 0xFFF14C4C, 0xFF23D18B, 0xFFF5F543, 0xFF3B8EEA, 0xFFD670D6, 0xFF29B8DB, 0xFFFFFFFF,
).map { Color(it) }

val TerminalAnsiLight = listOf(
    0xFF24292E, 0xFFD73A49, 0xFF22863A, 0xFFB08800, 0xFF0366D6, 0xFF6F42C1, 0xFF1B7C83, 0xFF6A737D,
    0xFF959DA5, 0xFFCB2431, 0xFF28A745, 0xFFDBAB09, 0xFF2188FF, 0xFF8A63D2, 0xFF3192AA, 0xFFD1D5DA,
).map { Color(it) }

/**
 * 画终端、接键盘的视图。点一下弹出键盘；上下拖动看回滚（全屏程序里改成发方向键）；双指缩放调整字号（9–24）；
 * 接鼠标或触控板时滚轮也能滚动；长按交给 [onLongPress]（复制、粘贴）。
 */
@SuppressLint("ViewConstructor")
internal class TerminalCanvasView(context: Context, var screen: TerminalScreen, textSizeSp: Float, var onLongPress: () -> Unit, var onTextSize: (Float) -> Unit) : View(context) {
    private var sizeSp = textSizeSp
    private var renderer = renderer(sizeSp)
    /** 显示的第一行：0 是当前屏幕，负数是往回滚了几行。 */
    private var topRow = 0
    private var scrollRemainder = 0f

    init {
        isFocusable = true
        isFocusableInTouchMode = true
    }

    private fun renderer(sp: Float) = TerminalRenderer(
        TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_SP, sp, resources.displayMetrics).roundToInt(), Typeface.MONOSPACE,
    )

    fun setTextSize(sp: Float) {
        val v = sp.coerceIn(9f, 24f)
        if (v == sizeSp) return
        sizeSp = v
        renderer = renderer(v)
        updateSize()
        onTextSize(v)
    }

    fun onOutput() {
        // 往回滚着看的时候，新输出不把视图拉回底部，但要跟着内容往上走。
        invalidate()
    }

    fun scrollToBottom() {
        if (topRow != 0) {
            topRow = 0
            invalidate()
        }
    }

    override fun onSizeChanged(w: Int, h: Int, oldw: Int, oldh: Int) = updateSize()

    private fun updateSize() {
        if (width == 0 || height == 0) return
        val cols = max(4, ((width - paddingLeft - paddingRight) / renderer.fontWidth).toInt())
        val rows = max(4, (height - paddingTop - paddingBottom) / renderer.fontLineSpacing)
        screen.sizeTo(cols, rows)
        topRow = 0
        invalidate()
    }

    override fun onDraw(canvas: Canvas) {
        canvas.drawColor(screen.background)
        val e = screen.emulator ?: return
        topRow = topRow.coerceIn(-e.screen.activeTranscriptRows, 0)
        canvas.save()
        canvas.translate(paddingLeft.toFloat(), paddingTop.toFloat())
        renderer.render(e, canvas, topRow, -1, -1, -1, -1)
        canvas.restore()
    }

    // ---- 手势 ----

    private val gestures = GestureDetector(context, object : GestureDetector.SimpleOnGestureListener() {
        override fun onDown(e: MotionEvent) = true

        override fun onSingleTapUp(e: MotionEvent): Boolean {
            showKeyboard()
            return true
        }

        override fun onScroll(e1: MotionEvent?, e2: MotionEvent, dx: Float, dy: Float): Boolean {
            if (scaling) return true
            scrollBy(dy)
            return true
        }

        override fun onLongPress(e: MotionEvent) {
            if (!scaling) onLongPress()
        }
    })

    private var scaling = false
    private val scale = ScaleGestureDetector(context, object : ScaleGestureDetector.SimpleOnScaleGestureListener() {
        override fun onScaleBegin(detector: ScaleGestureDetector): Boolean {
            scaling = true
            return true
        }

        override fun onScale(detector: ScaleGestureDetector): Boolean {
            val f = detector.scaleFactor
            if (f > 1.08f || f < 0.92f) {
                setTextSize(sizeSp * f)
                return true
            }
            return false
        }

        override fun onScaleEnd(detector: ScaleGestureDetector) {
            scaling = false
        }
    })

    /** 拖动 [dy] 像素：普通屏幕滚回滚；全屏程序（vim、less）用的备用屏幕没有回滚，发上下方向键。 */
    private fun scrollBy(dy: Float) {
        val e = screen.emulator ?: return
        scrollRemainder += dy
        val lines = (scrollRemainder / renderer.fontLineSpacing).toInt()
        if (lines == 0) return
        scrollRemainder -= lines * renderer.fontLineSpacing
        if (e.isAlternateBufferActive) {
            val key = if (lines < 0) KeyEvent.KEYCODE_DPAD_UP else KeyEvent.KEYCODE_DPAD_DOWN
            repeat(kotlin.math.abs(lines)) { screen.sendKey(key) }
        } else {
            topRow = (topRow + lines).coerceIn(-e.screen.activeTranscriptRows, 0)
            invalidate()
        }
    }

    @SuppressLint("ClickableViewAccessibility")
    override fun onTouchEvent(event: MotionEvent): Boolean {
        scale.onTouchEvent(event)
        gestures.onTouchEvent(event)
        if (event.actionMasked == MotionEvent.ACTION_UP || event.actionMasked == MotionEvent.ACTION_CANCEL) scrollRemainder = 0f
        return true
    }

    override fun onGenericMotionEvent(event: MotionEvent): Boolean {
        if (event.actionMasked == MotionEvent.ACTION_SCROLL) {
            scrollBy(-event.getAxisValue(MotionEvent.AXIS_VSCROLL) * renderer.fontLineSpacing * 3)
            return true
        }
        return super.onGenericMotionEvent(event)
    }

    fun showKeyboard() {
        requestFocus()
        context.getSystemService(InputMethodManager::class.java)?.showSoftInput(this, InputMethodManager.SHOW_IMPLICIT)
    }

    // ---- 键盘 ----

    override fun onCheckIsTextEditor() = true

    override fun onCreateInputConnection(outAttrs: EditorInfo): InputConnection {
        // 不要联想和自动更正：输入的是命令。
        outAttrs.inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD or InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
        outAttrs.imeOptions = EditorInfo.IME_FLAG_NO_FULLSCREEN or EditorInfo.IME_FLAG_NO_EXTRACT_UI
        return object : BaseInputConnection(this, true) {
            override fun commitText(text: CharSequence?, newCursorPosition: Int): Boolean {
                super.commitText(text, newCursorPosition)
                flush()
                return true
            }

            override fun finishComposingText(): Boolean {
                super.finishComposingText()
                flush()
                return true
            }

            override fun deleteSurroundingText(beforeLength: Int, afterLength: Int): Boolean {
                // 输入法的退格：编辑框里已经没有字了，发给终端。
                repeat(max(1, beforeLength)) { sendKeyEvent(KeyEvent(KeyEvent.ACTION_DOWN, KeyEvent.KEYCODE_DEL)) }
                return true
            }

            private fun flush() {
                val e = editable ?: return
                val text = e.toString()
                e.clear()
                typeText(text)
            }
        }
    }

    /** 键盘输入的文字：Ctrl、Alt 只作用于第一个字符。 */
    private fun typeText(text: String) {
        if (text.isEmpty()) return
        val first = text.codePointAt(0)
        val rest = text.substring(Character.charCount(first))
        typeCodePoint(first)
        if (rest.isNotEmpty()) screen.sendText(rest)
    }

    private fun typeCodePoint(cp: Int) {
        var c = cp
        if (screen.ctrl) {
            c = when (c) {
                in 'a'.code..'z'.code -> c - 'a'.code + 1
                in 'A'.code..'Z'.code -> c - 'A'.code + 1
                ' '.code, '2'.code, '@'.code -> 0
                '['.code, '3'.code -> 27
                '\\'.code, '4'.code -> 28
                ']'.code, '5'.code -> 29
                '^'.code, '6'.code -> 30
                '_'.code, '7'.code, '/'.code -> 31
                '8'.code -> 127
                else -> c
            }
        }
        val prefix = if (screen.alt) "\u001b" else ""
        screen.ctrl = false
        screen.alt = false
        screen.sendText(prefix + String(Character.toChars(c)))
    }

    override fun onKeyDown(keyCode: Int, event: KeyEvent): Boolean {
        val e = screen.emulator ?: return super.onKeyDown(keyCode, event)
        if (keyCode == KeyEvent.KEYCODE_BACK) return super.onKeyDown(keyCode, event)
        var mod = 0
        if (event.isCtrlPressed || screen.ctrl) mod = mod or KeyHandler.KEYMOD_CTRL
        if (event.isAltPressed || screen.alt) mod = mod or KeyHandler.KEYMOD_ALT
        if (event.isShiftPressed) mod = mod or KeyHandler.KEYMOD_SHIFT
        val code = KeyHandler.getCode(keyCode, mod, e.isCursorKeysApplicationMode, e.isKeypadApplicationMode)
        if (code != null) {
            screen.ctrl = false
            screen.alt = false
            screen.sendText(code)
            return true
        }
        // 实体键盘（平板的键盘）上按下的普通字符。
        val meta = event.metaState and (KeyEvent.META_CTRL_MASK or KeyEvent.META_ALT_MASK).inv()
        val cp = event.getUnicodeChar(meta)
        if (cp > 0) {
            if (event.isCtrlPressed) screen.ctrl = true
            if (event.isAltPressed) screen.alt = true
            typeCodePoint(cp)
            return true
        }
        return super.onKeyDown(keyCode, event)
    }
}

/**
 * 终端区域。[textSizeSp] 是字号（双指缩放改它，[onTextSize] 收到新的值，记在本机）；长按交给 [onLongPress]。
 * 进入时弹出键盘（[autoFocus]）。
 */
@Composable
fun TerminalPane(
    screen: TerminalScreen,
    modifier: Modifier = Modifier,
    textSizeSp: Float = 13f,
    autoFocus: Boolean = true,
    onTextSize: (Float) -> Unit = {},
    onLongPress: () -> Unit = {},
) {
    AndroidView(
        factory = { ctx ->
            TerminalCanvasView(ctx, screen, textSizeSp, onLongPress, onTextSize).also { v ->
                val pad = (12 * ctx.resources.displayMetrics.density).roundToInt()
                v.setPadding(pad, pad / 2, pad, pad / 2)
                screen.view = v
                if (autoFocus) v.post { v.showKeyboard() }
            }
        },
        update = { v ->
            v.screen = screen
            v.onLongPress = onLongPress
            v.onTextSize = onTextSize
            screen.view = v
            v.invalidate()
        },
        onRelease = { v -> if (screen.view === v) screen.view = null },
        modifier = modifier,
    )
}

/**
 * 键盘上方一行按键（docs/android.md「终端」）：Esc、Tab、Ctrl、Alt、方向键、`|`、`/`、`-`、`~`。每个键 44 × 44，
 * 放不下时这一行横向滚动。Ctrl、Alt 按一下亮起，作用于下一个键。点按键不抢走终端的焦点，键盘不收起。
 */
@Composable
fun TerminalKeys(screen: TerminalScreen, modifier: Modifier = Modifier) {
    val c = NagiTheme.colors
    Row(
        modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = 8.dp, vertical = 4.dp),
        horizontalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        @Composable
        fun key(label: String, on: Boolean = false, onClick: () -> Unit) {
            Box(
                Modifier.size(44.dp).clip(RoundedCornerShape(6.dp)).background(if (on) c.primary else c.muted)
                    .clickable(role = Role.Button, onClick = onClick),
                contentAlignment = Alignment.Center,
            ) {
                Text(label, style = NagiTheme.type.mono, color = if (on) c.primaryForeground else c.foreground)
            }
        }
        key("Esc") { screen.sendKey(KeyEvent.KEYCODE_ESCAPE) }
        key("Tab") { screen.sendKey(KeyEvent.KEYCODE_TAB) }
        key("Ctrl", screen.ctrl) { screen.ctrl = !screen.ctrl }
        key("Alt", screen.alt) { screen.alt = !screen.alt }
        key("←") { screen.sendKey(KeyEvent.KEYCODE_DPAD_LEFT) }
        key("↑") { screen.sendKey(KeyEvent.KEYCODE_DPAD_UP) }
        key("↓") { screen.sendKey(KeyEvent.KEYCODE_DPAD_DOWN) }
        key("→") { screen.sendKey(KeyEvent.KEYCODE_DPAD_RIGHT) }
        for (ch in listOf("|", "/", "-", "~")) key(ch) { typeSymbol(screen, ch) }
    }
}

private fun typeSymbol(screen: TerminalScreen, ch: String) {
    val prefix = if (screen.alt) "\u001b" else ""
    screen.ctrl = false
    screen.alt = false
    screen.sendText(prefix + ch)
}
