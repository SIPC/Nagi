package ink.sipc.nagi.data

import com.google.protobuf.ByteString
import ink.sipc.nagi.network.ElevationRequiredException
import ink.sipc.nagi.network.PanelException
import ink.sipc.nagi.network.UnreachableException
import ink.sipc.nagi.proto.v1.DeleteNodeFileRequest
import ink.sipc.nagi.proto.v1.FileOpEvent
import ink.sipc.nagi.proto.v1.FileOpResult
import ink.sipc.nagi.proto.v1.ListNodeDirResponse
import ink.sipc.nagi.proto.v1.NodeCompressRequest
import ink.sipc.nagi.proto.v1.NodeExtractRequest
import ink.sipc.nagi.proto.v1.NodeFileBackup
import ink.sipc.nagi.proto.v1.NodeFileRef
import ink.sipc.nagi.proto.v1.NodeTransferRequest
import ink.sipc.nagi.proto.v1.ReadNodeFileResponse
import ink.sipc.nagi.proto.v1.RestoreNodeFileBackupRequest
import ink.sipc.nagi.proto.v1.WriteNodeFileRequest
import ink.sipc.nagi.proto.v1.WriteNodeFileResponse
import com.connectrpc.Code
import com.connectrpc.ServerOnlyStreamInterface
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import okio.BufferedSink
import org.json.JSONObject
import java.io.InputStream
import java.io.OutputStream
import java.security.MessageDigest

// 节点文件（docs/node-access.md#文件、docs/android.md#文件）：和网页 pages/node-access.tsx 一样。浏览、读写小文本、删除、
// 新建、改名、移动、复制、压缩、解压走 NodeService；上传、下载走面板的 /api/node-file（App 带令牌直接下载，不用签名链接）。
// 都要「节点终端和文件」权限，并且先做过高权限确认（面板回 ElevationRequiredException）。

/** 一页最多列多少项（和网页一样）。 */
const val NODE_DIR_PAGE = 200

/** 能在线编辑的大小（1 MiB，和网页一样）。 */
const val NODE_EDIT_MAX = 1L shl 20

/** 上传时每块的大小（和网页一样 8 MiB）。 */
private const val CHUNK = 8 shl 20

fun joinPath(dir: String, name: String) = if (dir == "/") "/$name" else "$dir/$name"

fun parentPath(path: String): String = path.substring(0, path.lastIndexOf('/').coerceAtLeast(0)).ifEmpty { "/" }

/** 能解压的压缩包（zip、tar.gz、tgz、tar），和网页一样。 */
fun isArchive(name: String): Boolean {
    val n = name.lowercase()
    return listOf(".tar.gz", ".tgz", ".zip", ".tar").any { n.endsWith(it) && n.length > it.length }
}

/** 节点文件的接口。 */
class NodeFiles(private val session: PanelSession, private val nodeId: String) {
    private fun ref(path: String, offset: Long = 0, length: Long = 0) =
        NodeFileRef.newBuilder().setNodeId(nodeId).setPath(path).setOffset(offset).setLength(length).build()

    /** 列一页目录（[offset] 起最多 [NODE_DIR_PAGE] 项）。返回的 path 是解开符号链接以后的真实路径。 */
    suspend fun list(path: String, offset: Long = 0): ListNodeDirResponse =
        session.rpc { c, h -> c.nodes.listNodeDir(ref(path, offset, NODE_DIR_PAGE.toLong()), h) }

    suspend fun read(path: String): ReadNodeFileResponse = session.rpc { c, h -> c.nodes.readNodeFile(ref(path), h) }

    /** 保存：[expectedSha] 是打开时内容的 SHA-256，文件已经被别人改了就被拒绝（Code.ABORTED）。 */
    suspend fun write(path: String, content: ByteArray, expectedSha: String = "", create: Boolean = false, confirmed: Boolean = false): WriteNodeFileResponse =
        session.rpc { c, h ->
            c.nodes.writeNodeFile(
                WriteNodeFileRequest.newBuilder().setNodeId(nodeId).setPath(path).setContent(ByteString.copyFrom(content))
                    .setExpectedSha256(expectedSha).setCreate(create).setConfirmed(confirmed).build(),
                h,
            )
        }

    suspend fun mkdir(path: String) {
        session.rpc { c, h -> c.nodes.createNodeDir(ref(path), h) }
    }

    /** 删除。目录和敏感文件要带上名称（[confirmName]，界面上让用户输入）。 */
    suspend fun delete(path: String, confirmName: String = "") {
        session.rpc { c, h -> c.nodes.deleteNodeFile(DeleteNodeFileRequest.newBuilder().setNodeId(nodeId).setPath(path).setConfirmName(confirmName).build(), h) }
    }

    suspend fun rename(path: String, newName: String, confirmed: Boolean) {
        session.rpc { c, h ->
            c.nodes.renameNodeFile(
                NodeTransferRequest.newBuilder().setNodeId(nodeId).addPaths(path).setDestDir(parentPath(path)).setNewName(newName).setConfirmed(confirmed).build(),
                h,
            )
        }
    }

    /** 复制或移动到 [destDir]。[conflict] 第一次为空，有同名时节点返回 conflicts，再带 overwrite、rename、skip 重试。 */
    suspend fun transfer(paths: List<String>, destDir: String, copy: Boolean, conflict: String, confirmed: Boolean, progress: (FileOpEvent) -> Unit): FileOpResult {
        val req = NodeTransferRequest.newBuilder().setNodeId(nodeId).addAllPaths(paths).setDestDir(destDir).setCopy(copy).setConflict(conflict).setConfirmed(confirmed).build()
        return stream(req, progress) { c, h -> if (copy) c.nodes.copyNodeFiles(h) else c.nodes.moveNodeFiles(h) }
    }

    /** 把同一目录里的几项压缩成 [dest]（format 是 zip 或 tar.gz）。 */
    suspend fun compress(paths: List<String>, dest: String, format: String, conflict: String, confirmed: Boolean, progress: (FileOpEvent) -> Unit): FileOpResult {
        val req = NodeCompressRequest.newBuilder().setNodeId(nodeId).addAllPaths(paths).setDest(dest).setFormat(format).setConflict(conflict).setConfirmed(confirmed).build()
        return stream(req, progress) { c, h -> c.nodes.compressNodeFiles(h) }
    }

    suspend fun extract(path: String, destDir: String, intoNewDir: Boolean, conflict: String, confirmed: Boolean, progress: (FileOpEvent) -> Unit): FileOpResult {
        val req = NodeExtractRequest.newBuilder().setNodeId(nodeId).setPath(path).setDestDir(destDir).setIntoNewDir(intoNewDir).setConflict(conflict).setConfirmed(confirmed).build()
        return stream(req, progress) { c, h -> c.nodes.extractNodeFile(h) }
    }

    /** 长操作的流：边做边报进度，最后一条带结果。 */
    private suspend fun <Req> stream(
        req: Req,
        progress: (FileOpEvent) -> Unit,
        open: suspend (ink.sipc.nagi.network.PanelClient, Map<String, List<String>>) -> ServerOnlyStreamInterface<Req, FileOpEvent>,
    ): FileOpResult = session.tokens.call { h ->
        val s = open(session.client, h)
        var result: FileOpResult = FileOpResult.getDefaultInstance()
        try {
            s.sendAndClose(req).getOrThrow()
            for (ev in s.responseChannel()) {
                progress(ev)
                if (ev.hasResult()) result = ev.result
            }
        } finally {
            if (!s.isReceiveClosed()) s.receiveClose()
        }
        result
    }

    suspend fun backups(path: String): List<NodeFileBackup> =
        session.rpc { c, h -> c.nodes.listNodeFileBackups(ref(path), h) }.backupsList

    suspend fun restore(path: String, id: String, confirmed: Boolean) {
        session.rpc { c, h -> c.nodes.restoreNodeFileBackup(RestoreNodeFileBackupRequest.newBuilder().setNodeId(nodeId).setPath(path).setId(id).setConfirmed(confirmed).build(), h) }
    }

    // ---- 上传、下载（面板的 /api/node-file）----

    private fun url(path: String, confirmed: Boolean = false, params: Map<String, String> = emptyMap()): HttpUrl =
        "https://${session.panel.address}/api/node-file".toHttpUrl().newBuilder()
            .addQueryParameter("node", nodeId).addQueryParameter("path", path)
            .apply { if (confirmed) addQueryParameter("confirmed", "1") }
            .apply { params.forEach { (k, v) -> addQueryParameter(k, v) } }
            .build()

    /** 发一个请求，失败时换成 PanelException（令牌要重新换取、要先确认身份的处理和接口一致）。 */
    private suspend fun <T> http(build: (Request.Builder) -> Request.Builder, read: (Response) -> T): T = session.tokens.call { h ->
        withContext(Dispatchers.IO) {
            val req = build(Request.Builder()).apply { h.forEach { (k, v) -> v.forEach { addHeader(k, it) } } }.build()
            val res = try {
                session.client.http.newCall(req).execute()
            } catch (e: java.io.IOException) {
                throw UnreachableException(e.message ?: e.javaClass.simpleName, e)
            }
            res.use { r ->
                if (!r.isSuccessful) throw httpError(r)
                read(r)
            }
        }
    }

    /**
     * 下载 [path]（目录打包成 tar.gz）写到 [out]，[progress] 收到已写的字节数和总大小（不知道时为 -1）。
     * 返回面板给的文件名（Content-Disposition），没有时为 null。
     */
    suspend fun download(path: String, out: OutputStream, progress: (Long, Long) -> Unit): String? =
        http({ it.url(url(path)).get() }) { r ->
            val total = r.body.contentLength()
            val name = r.header("Content-Disposition")?.let { filenameOf(it) }
            r.body.byteStream().use { input -> copy(input, out, total, progress) }
            name
        }

    /**
     * 上传到 [path]（目录里的完整路径）。[size] 是总大小，[open] 每次调用都从头读一遍（先算哈希，再按块传）。
     * [chunked] 为 false 时是旧版本的节点，整个文件一次传（最大 8 GB）。覆盖敏感文件要 [confirmed]。
     */
    suspend fun upload(path: String, size: Long, chunked: Boolean, confirmed: Boolean, open: () -> InputStream, progress: (Long) -> Unit) {
        if (!chunked) {
            http({ b ->
                b.url(url(path, confirmed)).header("X-Nagi-Upload", "1").post(streamBody(size, open, progress))
            }) {}
            return
        }
        // 先算整个文件的 SHA-256：提交时核对，也用作断点续传的 key（同一个文件再传时接着传）。
        val whole = withContext(Dispatchers.IO) { open().use { sha256Hex(it) } }
        val key = sha256Hex("$path\n$size\n$whole".byteInputStream()).take(32)
        val begin = http({ b ->
            b.url(url(path, confirmed, mapOf("op" to "begin", "size" to size.toString(), "key" to key))).header("X-Nagi-Upload", "1")
                .post(ByteArray(0).toRequestBody(null))
        }) { r -> JSONObject(r.body.string()) }
        val id = begin.getString("id")
        var offset = begin.optLong("offset", 0)
        progress(offset)
        withContext(Dispatchers.IO) {
            open().use { input ->
                skipFully(input, offset)
                val buf = ByteArray(CHUNK)
                while (offset < size) {
                    val n = readFully(input, buf)
                    if (n <= 0) break
                    val chunk = buf.copyOf(n)
                    val sum = sha256Hex(chunk.inputStream())
                    val res = http({ b ->
                        b.url(url(path, confirmed, mapOf("op" to "part", "id" to id, "offset" to offset.toString())))
                            .header("X-Nagi-Upload", "1").header("X-Nagi-Chunk-Sha256", sum)
                            .put(chunk.toRequestBody("application/octet-stream".toMediaType()))
                    }) { r -> JSONObject(r.body.string()) }
                    offset = res.optLong("offset", offset + n)
                    progress(offset)
                }
            }
        }
        http({ b ->
            b.url(url(path, confirmed, mapOf("op" to "commit", "id" to id, "sha256" to whole))).header("X-Nagi-Upload", "1")
                .post(ByteArray(0).toRequestBody(null))
        }) {}
    }

    private fun streamBody(size: Long, open: () -> InputStream, progress: (Long) -> Unit) = object : RequestBody() {
        override fun contentType() = "application/octet-stream".toMediaType()
        override fun contentLength() = size
        override fun writeTo(sink: BufferedSink) {
            open().use { input ->
                val buf = ByteArray(64 shl 10)
                var done = 0L
                while (true) {
                    val n = input.read(buf)
                    if (n < 0) break
                    sink.write(buf, 0, n)
                    done += n
                    progress(done)
                }
            }
        }
    }
}

private fun copy(input: InputStream, out: OutputStream, total: Long, progress: (Long, Long) -> Unit) {
    val buf = ByteArray(64 shl 10)
    var done = 0L
    var last = 0L
    while (true) {
        val n = input.read(buf)
        if (n < 0) break
        out.write(buf, 0, n)
        done += n
        if (done - last >= 256 shl 10) {
            last = done
            progress(done, total)
        }
    }
    progress(done, total)
}

private fun readFully(input: InputStream, buf: ByteArray): Int {
    var off = 0
    while (off < buf.size) {
        val n = input.read(buf, off, buf.size - off)
        if (n < 0) break
        off += n
    }
    return off
}

private fun skipFully(input: InputStream, n: Long) {
    var left = n
    val buf = ByteArray(64 shl 10)
    while (left > 0) {
        val r = input.read(buf, 0, minOf(buf.size.toLong(), left).toInt())
        if (r < 0) break
        left -= r
    }
}

private fun sha256Hex(input: InputStream): String {
    val md = MessageDigest.getInstance("SHA-256")
    val buf = ByteArray(64 shl 10)
    while (true) {
        val n = input.read(buf)
        if (n < 0) break
        md.update(buf, 0, n)
    }
    return md.digest().joinToString("") { "%02x".format(it) }
}

/** Content-Disposition 里的文件名（filename*=UTF-8'' 优先）。 */
internal fun filenameOf(header: String): String? {
    Regex("""filename\*=UTF-8''([^;]+)""", RegexOption.IGNORE_CASE).find(header)?.let {
        return java.net.URLDecoder.decode(it.groupValues[1], "UTF-8")
    }
    return Regex("""filename="?([^";]+)"?""", RegexOption.IGNORE_CASE).find(header)?.groupValues?.get(1)
}

/** /api/node-file 的错误：JSON 的 message；要先确认身份时换成 ElevationRequiredException。 */
private fun httpError(r: Response): PanelException {
    val msg = runCatching { JSONObject(r.body.string()).optString("message") }.getOrNull().orEmpty().ifEmpty { r.message.ifEmpty { "HTTP ${r.code}" } }
    if (r.code == 403 && r.header("X-Nagi-Elevation") == "required") return ElevationRequiredException(msg, null)
    val code = when (r.code) {
        401 -> Code.UNAUTHENTICATED
        403 -> Code.PERMISSION_DENIED
        404 -> Code.NOT_FOUND
        409 -> Code.ABORTED
        412 -> Code.FAILED_PRECONDITION
        429, 507 -> Code.RESOURCE_EXHAUSTED
        in 500..599 -> Code.UNAVAILABLE
        else -> Code.UNKNOWN
    }
    return PanelException(code, msg, r.header("Nagi-App-Retoken"))
}
