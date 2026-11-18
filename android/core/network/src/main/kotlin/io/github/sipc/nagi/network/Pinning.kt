package io.github.sipc.nagi.network

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

/** 证书的公钥指纹不是预期的面板。 */
class FingerprintMismatchException(val expected: String, val actual: String) :
    CertificateException("公钥指纹不一致：应为 $expected，实际为 $actual")

fun spkiFingerprint(cert: X509Certificate): String {
    val sum = MessageDigest.getInstance("SHA-256").digest(cert.publicKey.encoded)
    return "sha256//" + Base64.encodeToString(sum, Base64.NO_WRAP)
}

/** 只认指定公钥指纹的 TrustManager。 */
class PinnedTrustManager(private val fingerprint: String) : X509TrustManager {
    override fun checkClientTrusted(chain: Array<out X509Certificate>, authType: String) = throw CertificateException()

    override fun checkServerTrusted(chain: Array<out X509Certificate>, authType: String) {
        val leaf = chain.firstOrNull() ?: throw CertificateException("面板没有提供证书")
        val got = spkiFingerprint(leaf)
        if (got != fingerprint) throw FingerprintMismatchException(fingerprint, got)
    }

    override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
}

/** 连接面板用的 OkHttp：校验公钥指纹，带上界面语言。长连接（订阅流）不设读超时。 */
fun pinnedClient(fingerprint: String): OkHttpClient {
    val tm = PinnedTrustManager(fingerprint)
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
