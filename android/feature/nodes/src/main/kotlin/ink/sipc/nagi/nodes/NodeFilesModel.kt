package ink.sipc.nagi.nodes

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.connectrpc.Code
import ink.sipc.nagi.data.NodeFiles
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.data.joinPath
import ink.sipc.nagi.network.ElevationRequiredException
import ink.sipc.nagi.network.PanelException
import ink.sipc.nagi.network.UnreachableException
import ink.sipc.nagi.proto.v1.FileOpResult
import ink.sipc.nagi.proto.v1.NodeFileEntry
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

// 节点文件页的状态（docs/android.md#文件）：当前目录、列表、多选、剪贴板、正在做的长操作。放在 ViewModel 里，切到别的标签再回来还在原来的目录。

/** 正在做的长操作（复制、移动、压缩、解压、上传、下载）：[label] 是说明，[done]、[total] 是进度（不知道总量时 total 为 -1）。 */
data class FileProgress(val label: String, val done: Long = 0, val total: Long = -1, val files: Long = 0)

/** 复制、剪切的几项：[dir] 是它们所在的目录。 */
data class FileClip(val dir: String, val names: List<String>, val cut: Boolean)

class NodeFilesViewModel(session: PanelSession, nodeId: String) : ViewModel() {
    val files = NodeFiles(session, nodeId)

    var path by mutableStateOf("/")
        private set
    /** 列出来的目录（解开符号链接以后的真实路径），和 [path] 不同时界面显示真实路径。 */
    var real by mutableStateOf("/")
        private set
    var entries by mutableStateOf<List<NodeFileEntry>?>(null)
        private set
    /** 目录的访问规则：normal、confirm（保存前要确认）、readonly（容器目录）。 */
    var access by mutableStateOf("normal")
        private set
    var truncated by mutableStateOf(false)
        private set
    var loadError by mutableStateOf<String?>(null)
        private set
    /** 要先验证身份（取消了系统验证框）：显示「验证」。 */
    var needVerify by mutableStateOf(false)
        private set
    var selected by mutableStateOf<Set<String>>(emptySet())
    var clip by mutableStateOf<FileClip?>(null)
    var progress by mutableStateOf<FileProgress?>(null)
    var error by mutableStateOf<List<String>>(emptyList())
    private var loadJob: Job? = null
    private var opJob: Job? = null
    /** 系统验证框（主界面提供），进入文件页和确认过期时弹出。 */
    var elevate: (suspend () -> Boolean)? = null

    val selecting: Boolean get() = selected.isNotEmpty()

    /** 操作结果的提示（Snackbar），由页面接上。 */
    var onMessage: (String) -> Unit = {}

    fun message(text: String) = onMessage(text)

    /** 打开目录 [p]（换目录时清掉多选）。 */
    fun open(p: String) {
        if (p != path) selected = emptySet()
        path = p
        load()
    }

    fun load(more: Boolean = false) {
        loadJob?.cancel()
        loadError = null
        if (!more) needVerify = false
        loadJob = viewModelScope.launch {
            try {
                val res = elevated { files.list(path, if (more) entries?.size?.toLong() ?: 0 else 0) }
                real = res.path.ifEmpty { path }
                access = res.access.ifEmpty { "normal" }
                truncated = res.truncated
                val list = res.entriesList.sortedWith(compareBy<NodeFileEntry> { !it.isDir }.thenBy(String.CASE_INSENSITIVE_ORDER) { it.name })
                entries = if (more) entries.orEmpty() + list else list
            } catch (e: CancellationException) {
                throw e
            } catch (e: ElevationRequiredException) {
                needVerify = true
            } catch (e: Exception) {
                loadError = e.message.orEmpty()
            }
        }
    }

    /** 面板要求先确认身份时弹系统验证框，确认后再做一次；取消时抛出原来的错误。 */
    suspend fun <T> elevated(block: suspend () -> T): T = try {
        block()
    } catch (e: ElevationRequiredException) {
        val ask = elevate
        if (ask != null && ask()) block() else throw e
    }

    fun toggle(name: String) {
        selected = if (name in selected) selected - name else selected + name
    }

    /**
     * 做一个长操作：第一次不指定同名处理，节点报告同名时问 [askConflict]（覆盖、保留两者、跳过，取消时 null）再做一次。
     * [run] 拿到同名处理的方式，返回节点的结果。完成后刷新列表，[done] 收到跳过的项数。
     */
    fun op(
        label: String,
        unreachable: String,
        askConflict: suspend (List<String>) -> String?,
        run: suspend (conflict: String, progress: (Long, Long) -> Unit) -> FileOpResult,
        done: (FileOpResult) -> Unit = {},
    ) {
        if (opJob?.isActive == true) return
        error = emptyList()
        opJob = viewModelScope.launch {
            progress = FileProgress(label)
            try {
                val tick: (Long, Long) -> Unit = { bytes, n -> progress = FileProgress(label, bytes, -1, n) }
                var res = elevated { run("", tick) }
                if (res.conflictsCount > 0) {
                    progress = null
                    val choice = askConflict(res.conflictsList) ?: return@launch
                    progress = FileProgress(label)
                    res = elevated { run(choice, tick) }
                }
                done(res)
            } catch (e: CancellationException) {
                throw e
            } catch (e: UnreachableException) {
                error = listOf(unreachable)
            } catch (e: Exception) {
                error = listOf(e.message.orEmpty())
            } finally {
                progress = null
                load()
            }
        }
    }

    /** 做一个短操作（新建、改名、删除）：出错写在列表上面，做完刷新。 */
    fun quick(unreachable: String, block: suspend () -> Unit) {
        error = emptyList()
        viewModelScope.launch {
            try {
                elevated { block() }
            } catch (e: CancellationException) {
                throw e
            } catch (e: UnreachableException) {
                error = listOf(unreachable)
            } catch (e: Exception) {
                error = listOf(e.message.orEmpty())
            } finally {
                load()
            }
        }
    }

    /** 传输（上传、下载）：带进度，可以取消。 */
    fun transfer(label: String, unreachable: String, block: suspend ((Long, Long) -> Unit) -> Unit) {
        if (opJob?.isActive == true) return
        error = emptyList()
        opJob = viewModelScope.launch {
            progress = FileProgress(label)
            try {
                elevated { block { done, total -> progress = FileProgress(label, done, total) } }
            } catch (e: CancellationException) {
                throw e
            } catch (e: UnreachableException) {
                error = listOf(unreachable)
            } catch (e: Exception) {
                error = listOf(e.message.orEmpty())
            } finally {
                progress = null
                load()
            }
        }
    }

    fun cancelOp() {
        opJob?.cancel()
        progress = null
    }

    fun pathOf(name: String) = joinPath(real, name)

    /** 改敏感路径时节点要确认（FAILED_PRECONDITION）：[ask] 问过以后带 confirmed 再做一次。 */
    suspend fun <T> sensitive(known: Boolean, ask: suspend () -> Boolean, block: suspend (Boolean) -> T): T {
        var confirmed = false
        if (known) {
            if (!ask()) throw CancellationException()
            confirmed = true
        }
        return try {
            block(confirmed)
        } catch (e: PanelException) {
            if (confirmed || e.code != Code.FAILED_PRECONDITION || e is ElevationRequiredException) throw e
            if (!ask()) throw CancellationException()
            block(true)
        }
    }
}
