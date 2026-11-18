package ink.sipc.nagi.network

import android.util.Base64
import java.security.MessageDigest
import java.security.SecureRandom
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.util.Locale
import java.util.concurrent.TimeUnit
import javax.net.ssl.SSLContext
import javax.net.ssl.X509TrustManager
import okhttp3.OkHttpClient
import okhttp3.Protocol

// 面板用自签证书（见 docs/android.md#面板地址和备用面板）：只按公钥指纹校验，和节点连接面板一样。
// 指纹的格式和面板的 tlsutil.Fingerprint 一致：sha256// 加证书公钥（SPKI）的 SHA-256 的 base64。
// 扫码添加时二维码里只有前 128 位（sha256-128// 加 base64url，二维码短一些好扫），连上以后记下完整的指纹。

/** 完整公钥指纹的前缀。 */
const val PIN_FULL = "sha256//"

/** 只有前 128 位的公钥指纹的前缀。 */
const val PIN_128 = "sha256-128//"

/** 证书的公钥指纹不是预期的面板。 */
class FingerprintMismatchException(val expected: String, val actual: String) :
    CertificateException("公钥指纹不一致：应为 $expected，实际为 $actual")

fun spkiFingerprint(cert: X509Certificate): String = PIN_FULL + Base64.encodeToString(spkiSha256(cert), Base64.NO_WRAP)

private fun spkiSha256(cert: X509Certificate): ByteArray = MessageDigest.getInstance("SHA-256").digest(cert.publicKey.encoded)

/** 公钥指纹是否符合 [pin]：完整指纹要完全一样，只有前 128 位的比前 16 字节。 */
fun pinMatches(pin: String, sha256: ByteArray): Boolean = when {
    pin.startsWith(PIN_128) -> {
        val want = runCatching { Base64.decode(pin.removePrefix(PIN_128), Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP) }.getOrNull()
        want != null && want.size == 16 && MessageDigest.isEqual(want, sha256.copyOf(16))
    }
    else -> pin == PIN_FULL + Base64.encodeToString(sha256, Base64.NO_WRAP)
}

/** 只认指定公钥指纹的 TrustManager。[seen] 是最近一次校验通过的证书的完整指纹。 */
class PinnedTrustManager(private val fingerprint: String) : X509TrustManager {
    @Volatile
    var seen: String? = null
        private set

    override fun checkClientTrusted(chain: Array<out X509Certificate>, authType: String) = throw CertificateException()

    override fun checkServerTrusted(chain: Array<out X509Certificate>, authType: String) {
        val leaf = chain.firstOrNull() ?: throw CertificateException("面板没有提供证书")
        val sum = spkiSha256(leaf)
        val got = PIN_FULL + Base64.encodeToString(sum, Base64.NO_WRAP)
        if (!pinMatches(fingerprint, sum)) throw FingerprintMismatchException(fingerprint, got)
        seen = got
    }

    override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
}

/** 连接面板用的 OkHttp：校验公钥指纹，带上界面语言。长连接（订阅流）不设读超时。 */
fun pinnedClient(fingerprint: String, tm: PinnedTrustManager = PinnedTrustManager(fingerprint)): OkHttpClient {
    val ssl = SSLContext.getInstance("TLS").apply { init(null, arrayOf(tm), SecureRandom()) }
    return OkHttpClient.Builder()
        .sslSocketFactory(ssl.socketFactory, tm)
        // 证书里没有面板的 IP 或域名也没关系：公钥指纹已经确认了是这个面板。
        .hostnameVerifier { _, _ -> true }
        .protocols(listOf(Protocol.HTTP_2, Protocol.HTTP_1_1))
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(0, TimeUnit.SECONDS)
        .writeTimeout(0, TimeUnit.SECONDS)
        .retryOnConnectionFailure(true)
        .addInterceptor { chain ->
            val lang = if (Locale.getDefault().language == "zh") "zh-CN" else "en"
            chain.proceed(chain.request().newBuilder().header("Accept-Language", lang).build())
        }
        .build()
}
