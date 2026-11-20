package ink.sipc.nagi.ui

import android.content.Context
import android.graphics.Typeface
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
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
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import io.github.rosemoe.sora.event.ContentChangeEvent
import io.github.rosemoe.sora.langs.textmate.TextMateColorScheme
import io.github.rosemoe.sora.langs.textmate.TextMateLanguage
import io.github.rosemoe.sora.langs.textmate.registry.FileProviderRegistry
import io.github.rosemoe.sora.langs.textmate.registry.GrammarRegistry
import io.github.rosemoe.sora.langs.textmate.registry.ThemeRegistry
import io.github.rosemoe.sora.langs.textmate.registry.model.ThemeModel
import io.github.rosemoe.sora.langs.textmate.registry.provider.AssetsFileResolver
import io.github.rosemoe.sora.widget.CodeEditor
import io.github.rosemoe.sora.widget.schemes.EditorColorScheme
import org.eclipse.tm4e.core.registry.IThemeSource

// 编辑器（docs/android.md#日志终端和编辑器）：Sora Editor，等宽字体、行号、撤销和重做；YAML、JSON、Shell、INI、Dockerfile
// 按语法着色（TextMate 语法在 assets/textmate，取自 VS Code），其他文件按纯文本显示。编辑区的底色、行号、选区用 App 的颜色。

/** 按文件名选语法：返回 TextMate 的 scopeName，认不出时为 null（纯文本）。 */
fun editorLanguageFor(fileName: String): String? {
    val name = fileName.lowercase()
    val ext = name.substringAfterLast('.', "")
    return when {
        name == "dockerfile" || name.startsWith("dockerfile.") || ext == "dockerfile" || name == "containerfile" -> "source.dockerfile"
        ext == "yml" || ext == "yaml" -> "source.yaml"
        ext == "json" || name == ".dockerignore.json" -> "source.json"
        ext == "sh" || ext == "bash" || ext == "zsh" || name == ".bashrc" || name == ".profile" || name == ".bash_profile" || name == ".zshrc" -> "source.shell"
        ext == "ini" || ext == "conf" || ext == "cfg" || ext == "env" || name == ".env" || ext == "toml" || ext == "properties" || ext == "service" -> "source.ini"
        else -> null
    }
}

private object TextMate {
    @Volatile private var ready = false

    /** 第一次用时加载语法和两套配色。 */
    fun init(context: Context) {
        if (ready) return
        synchronized(this) {
            if (ready) return
            FileProviderRegistry.getInstance().addFileProvider(AssetsFileResolver(context.applicationContext.assets))
            GrammarRegistry.getInstance().loadGrammars("textmate/languages.json")
            for ((name, dark) in listOf("nagi-light" to false, "nagi-dark" to true)) {
                val path = "textmate/$name.json"
                val input = FileProviderRegistry.getInstance().tryGetInputStream(path) ?: continue
                ThemeRegistry.getInstance().loadTheme(ThemeModel(IThemeSource.fromInputStream(input, path, null), name).apply { isDark = dark })
            }
            ready = true
        }
    }
}

/** 一个编辑器：拿到里面的文字、撤销和重做、在光标处输入（键盘上方的按键）。 */
class CodeEditorState {
    internal var editor: CodeEditor? = null
    var canUndo by mutableStateOf(false)
        private set
    var canRedo by mutableStateOf(false)
        private set

    val text: String get() = editor?.text?.toString().orEmpty()

    fun undo() {
        editor?.undo()
        refresh()
    }

    fun redo() {
        editor?.redo()
        refresh()
    }

    /** 在光标处输入 [text]，光标停在 [cursorBack] 个字符之前（成对的括号停在中间）。 */
    fun insert(text: String, cursorBack: Int = 0) {
        val e = editor ?: return
        e.insertText(text, text.length - cursorBack)
    }

    fun indent() {
        editor?.indentOrCommitTab()
    }

    fun unindent() {
        editor?.unindentSelection()
    }

    internal fun refresh() {
        val e = editor ?: return
        canUndo = e.canUndo()
        canRedo = e.canRedo()
    }
}

/**
 * 编辑器区域。[initial] 是打开时的内容，[language] 是 [editorLanguageFor] 的结果；内容变了调用 [onChange]。
 * [readOnly] 时只能看和选择（只读的容器目录、二进制文件以外的大文件不在这里打开）。
 */
@Composable
fun CodeEditorPane(
    state: CodeEditorState,
    initial: String,
    language: String?,
    modifier: Modifier = Modifier,
    readOnly: Boolean = false,
    onChange: () -> Unit = {},
) {
    val c = NagiTheme.colors
    val dark = c.background.luminance() < 0.5f
    AndroidView(
        factory = { ctx ->
            TextMate.init(ctx)
            CodeEditor(ctx).also { e ->
                e.typefaceText = Typeface.MONOSPACE
                e.typefaceLineNumber = Typeface.MONOSPACE
                e.setTextSize(13f)
                e.isWordwrap = false
                e.setText(initial)
                e.subscribeEvent(ContentChangeEvent::class.java) { _, _ ->
                    state.refresh()
                    onChange()
                }
                state.editor = e
            }
        },
        update = { e ->
            ThemeRegistry.getInstance().setTheme(if (dark) "nagi-dark" else "nagi-light")
            val scheme = TextMateColorScheme.create(ThemeRegistry.getInstance())
            scheme.setColor(EditorColorScheme.WHOLE_BACKGROUND, c.background.toArgb())
            scheme.setColor(EditorColorScheme.LINE_NUMBER_BACKGROUND, c.background.toArgb())
            scheme.setColor(EditorColorScheme.LINE_NUMBER, c.mutedForeground.toArgb())
            scheme.setColor(EditorColorScheme.LINE_NUMBER_CURRENT, c.foreground.toArgb())
            scheme.setColor(EditorColorScheme.LINE_DIVIDER, c.border.toArgb())
            scheme.setColor(EditorColorScheme.CURRENT_LINE, c.muted.toArgb())
            scheme.setColor(EditorColorScheme.SELECTION_INSERT, c.foreground.toArgb())
            scheme.setColor(EditorColorScheme.SELECTION_HANDLE, c.foreground.toArgb())
            scheme.setColor(EditorColorScheme.SELECTED_TEXT_BACKGROUND, c.foreground.copy(alpha = 0.18f).toArgb())
            e.colorScheme = scheme
            if (language != null) e.setEditorLanguage(TextMateLanguage.create(language, true))
            e.editable = !readOnly
            state.editor = e
        },
        onRelease = { e ->
            if (state.editor === e) state.editor = null
            e.release()
        },
        modifier = modifier.clipToBounds(),
    )
}

/**
 * 键盘上方一行按键（docs/android.md「编辑器」）：Tab、缩进、取消缩进、`:`、`-`、`"`、`#`、`{}`、`[]`、撤销、重做，
 * 尺寸和滚动同终端的按键行。
 */
@Composable
fun EditorKeys(state: CodeEditorState, labels: EditorKeyLabels, modifier: Modifier = Modifier) {
    val c = NagiTheme.colors
    Row(
        modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = 8.dp, vertical = 4.dp),
        horizontalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        @Composable
        fun key(label: String, enabled: Boolean = true, onClick: () -> Unit) {
            Box(
                Modifier.size(44.dp).clip(RoundedCornerShape(6.dp)).background(c.muted)
                    .clickable(enabled = enabled, role = Role.Button, onClick = onClick),
                contentAlignment = Alignment.Center,
            ) {
                Text(label, style = NagiTheme.type.mono, color = if (enabled) c.foreground else c.mutedForeground, maxLines = 1)
            }
        }
        key("Tab") { state.insert("\t") }
        key("→|") { state.indent() }
        key("|←") { state.unindent() }
        key(":") { state.insert(":") }
        key("-") { state.insert("-") }
        key("\"") { state.insert("\"") }
        key("#") { state.insert("#") }
        key("{}") { state.insert("{}", 1) }
        key("[]") { state.insert("[]", 1) }
        key(labels.undo, state.canUndo) { state.undo() }
        key(labels.redo, state.canRedo) { state.redo() }
    }
}

/** 按键行上要翻译的两个键（读屏和显示）。 */
class EditorKeyLabels(val undo: String, val redo: String)

/** 保存前的差异里的一行：[kind] 是 ' '（不变）、'-'（删掉）、'+'（加上）。 */
data class DiffLine(val kind: Char, val text: String)

/**
 * 逐行比较 [old] 和 [new]（最长公共子序列），只留改动的行和前后各 [context] 行，中间省略的用 kind '…' 表示。
 * 太长（任一边超过 4000 行）时不逐行比，整段写成删掉和加上。
 */
fun lineDiff(old: String, new: String, context: Int = 2): List<DiffLine> {
    val a = old.split('\n')
    val b = new.split('\n')
    // 去掉相同的头尾，只比中间。
    var start = 0
    while (start < a.size && start < b.size && a[start] == b[start]) start++
    var endA = a.size
    var endB = b.size
    while (endA > start && endB > start && a[endA - 1] == b[endB - 1]) {
        endA--
        endB--
    }
    val midA = a.subList(start, endA)
    val midB = b.subList(start, endB)
    val core = ArrayList<DiffLine>()
    if (midA.size > 4000 || midB.size > 4000 || midA.size.toLong() * midB.size > 4_000_000L) {
        midA.forEach { core += DiffLine('-', it) }
        midB.forEach { core += DiffLine('+', it) }
    } else {
        val n = midA.size
        val m = midB.size
        val lcs = Array(n + 1) { IntArray(m + 1) }
        for (i in n - 1 downTo 0) for (j in m - 1 downTo 0) {
            lcs[i][j] = if (midA[i] == midB[j]) lcs[i + 1][j + 1] + 1 else maxOf(lcs[i + 1][j], lcs[i][j + 1])
        }
        var i = 0
        var j = 0
        while (i < n && j < m) {
            when {
                midA[i] == midB[j] -> { core += DiffLine(' ', midA[i]); i++; j++ }
                lcs[i + 1][j] >= lcs[i][j + 1] -> { core += DiffLine('-', midA[i]); i++ }
                else -> { core += DiffLine('+', midB[j]); j++ }
            }
        }
        while (i < n) core += DiffLine('-', midA[i++])
        while (j < m) core += DiffLine('+', midB[j++])
    }
    val all = a.subList(0, start).map { DiffLine(' ', it) } + core + a.subList(endA, a.size).map { DiffLine(' ', it) }
    // 只留改动附近的行。
    val keep = BooleanArray(all.size)
    all.forEachIndexed { idx, l ->
        if (l.kind != ' ') for (k in maxOf(0, idx - context)..minOf(all.size - 1, idx + context)) keep[k] = true
    }
    val out = ArrayList<DiffLine>()
    var skipped = false
    all.forEachIndexed { idx, l ->
        if (keep[idx]) {
            if (skipped && out.isNotEmpty()) out += DiffLine('…', "")
            skipped = false
            out += l
        } else {
            skipped = true
        }
    }
    return out
}
