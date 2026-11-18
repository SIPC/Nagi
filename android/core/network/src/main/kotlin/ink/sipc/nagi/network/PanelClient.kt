package ink.sipc.nagi.network

import com.connectrpc.Code
import com.connectrpc.ConnectException
import com.connectrpc.ProtocolClientConfig
import com.connectrpc.ResponseMessage
import com.connectrpc.extensions.GoogleJavaLiteProtobufStrategy
import com.connectrpc.impl.ProtocolClient
import com.connectrpc.okhttp.ConnectOkHttpClient
import com.connectrpc.protocols.NetworkProtocol
import ink.sipc.nagi.proto.v1.AppServiceClient
import ink.sipc.nagi.proto.v1.NodeServiceClient
import ink.sipc.nagi.proto.v1.ProjectServiceClient
import java.io.IOException
import javax.net.ssl.SSLException

/** 连接一个面板的客户端：地址是 对外地址:端口（按公钥指纹校验）。 */
class PanelClient(val address: String, fingerprint: String) {
    val http = pinnedClient(fingerprint)
    private val protocol = ProtocolClient(
        ConnectOkHttpClient(http),
        ProtocolClientConfig(
            host = "https://$address",
            serializationStrategy = GoogleJavaLiteProtobufStrategy(),
            networkProtocol = NetworkProtocol.CONNECT,
        ),
    )
    val app = AppServiceClient(protocol)
    val projects = ProjectServiceClient(protocol)
    val nodes = NodeServiceClient(protocol)
}

/** 面板返回的错误。message 是面板给的说明（已按界面语言），可以直接显示。 */
open class PanelException(val code: Code, message: String, val retoken: String? = null, cause: Throwable? = null) : Exception(message, cause)

/** 连不上面板（网络、TLS、公钥指纹不对）。 */
class UnreachableException(message: String, cause: Throwable?) : PanelException(Code.UNAVAILABLE, message, null, cause)

/** 需要解锁 App（用确认密钥换取）。 */
class UnlockRequiredException(message: String) : PanelException(Code.FAILED_PRECONDITION, message, "unlock")

/** 这台手机已经退出（设备记录不在了），回到欢迎页。 */
class DeviceRemovedException(message: String) : PanelException(Code.NOT_FOUND, message)

/** 响应头里的 Nagi-App-Retoken。 */
private fun ConnectException.retoken(): String? =
    metadata.entries.firstOrNull { it.key.equals("nagi-app-retoken", ignoreCase = true) }?.value?.firstOrNull()

/** 把 connect 的错误换成上面几种。 */
fun Throwable.toPanelException(): PanelException {
    if (this is PanelException) return this
    val ce = this as? ConnectException
    val cause = ce?.exception ?: this
    if (ce == null || ce.code == Code.UNAVAILABLE && cause is IOException || cause is SSLException || cause is FingerprintMismatchException) {
        val fp = generateSequence(cause) { it.cause }.filterIsInstance<FingerprintMismatchException>().firstOrNull()
        return UnreachableException(fp?.message ?: cause.message ?: cause.javaClass.simpleName, cause)
    }
    return PanelException(ce.code, ce.message ?: ce.code.codeName, ce.retoken(), ce)
}

/** 取出成功的回复，失败时抛 PanelException。 */
fun <T> ResponseMessage<T>.orThrow(): T = when (this) {
    is ResponseMessage.Success -> message
    is ResponseMessage.Failure -> throw cause.toPanelException()
}
