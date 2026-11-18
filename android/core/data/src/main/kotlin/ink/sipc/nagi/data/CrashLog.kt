package ink.sipc.nagi.data

import android.content.Context
import android.os.Build
import java.io.File
import java.io.PrintWriter
import java.io.StringWriter
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/** 一次崩溃：时间（毫秒）和第一行（异常类型和说明）。 */
data class CrashEntry(val at: Long, val title: String, internal val file: File)

/**
 * 崩溃日志（见 docs/android.md#工程「崩溃日志」）：只存在本机，不上报任何地方。最多保留最近 [KEEP] 次。
 * 只记异常和调用栈，去掉里面的地址、令牌、设备 ID 这类内容（[scrub]），不记集群数据。
 */
class CrashLog(context: Context, private val appVersion: String) {
    private val dir = File(context.applicationContext.filesDir, "crash")

    /** 在 Application 里调用一次：崩溃时先写日志，再交给系统原来的处理（结束进程、弹出提示）。 */
    fun install() {
        val previous = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { thread, error ->
            runCatching { write(thread.name, error) }
            previous?.uncaughtException(thread, error)
        }
    }

    fun list(): List<CrashEntry> = (dir.listFiles { f -> f.name.endsWith(".txt") } ?: emptyArray())
        .sortedByDescending { it.name }
        .map { f -> CrashEntry(f.name.removeSuffix(".txt").toLongOrNull() ?: f.lastModified(), title(f), f) }

    fun read(entry: CrashEntry): String = runCatching { entry.file.readText() }.getOrDefault("")

    fun clear() {
        dir.listFiles()?.forEach { it.delete() }
    }

    private fun title(f: File): String = runCatching {
        f.useLines { lines -> lines.dropWhile { it.isNotBlank() }.firstOrNull { it.isNotBlank() } }
    }.getOrNull().orEmpty()

    private fun write(thread: String, error: Throwable) {
        dir.mkdirs()
        val now = System.currentTimeMillis()
        val trace = StringWriter().also { error.printStackTrace(PrintWriter(it)) }.toString()
        val text = buildString {
            appendLine("time " + SimpleDateFormat("yyyy-MM-dd HH:mm:ss", Locale.ROOT).format(Date(now)))
            appendLine("app $appVersion")
            appendLine("android ${Build.VERSION.RELEASE} (SDK ${Build.VERSION.SDK_INT}) · ${Build.MANUFACTURER} ${Build.MODEL}")
            appendLine("thread $thread")
            appendLine()
            append(scrub(trace))
        }
        File(dir, "$now.txt").writeText(text)
        dir.listFiles { f -> f.name.endsWith(".txt") }?.sortedByDescending { it.name }?.drop(KEEP)?.forEach { it.delete() }
    }

    companion object {
        const val KEEP = 10

        private val rules = listOf(
            Regex("""https?://\S+""") to "<url>",
            Regex("""nagi_app_\S+""") to "<token>",
            Regex("""\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b""") to "<ip>",
            Regex("""\[?\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{0,4}\b\]?""") to "<ip>",
            // 主机名只在常见的写法里出现（unable to resolve host "…"、hostname …），包名不动。
            Regex("""(?i)(host(?:name)?\s*[=:]?\s*)"?[\w.-]+"?""") to "$1<host>",
            // 设备 ID、挑战、指纹这类随机串：20 个字符以上、同时有字母和数字。
            Regex("""\b(?=[A-Za-z0-9_+/=-]*\d)(?=[A-Za-z0-9_+/=-]*[A-Za-z])[A-Za-z0-9_+/=-]{20,}\b""") to "<redacted>",
        )

        /** 去掉日志里的地址、令牌、设备 ID 这类内容。 */
        fun scrub(text: String): String = rules.fold(text) { acc, (re, to) -> re.replace(acc, to) }
    }
}
