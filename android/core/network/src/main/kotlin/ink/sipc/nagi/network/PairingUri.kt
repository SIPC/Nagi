package ink.sipc.nagi.network

import android.net.Uri

/**
 * 网页「添加手机」二维码的内容（见 docs/android.md#扫码）：
 * nagi-app://pair?v=2&a=<对外地址:面板端口>&d=<面板域名>&k=<公钥指纹的前 128 位，base64url>&c=<配对码>。
 * 也认旧版面板的 v=1（fp=<完整的公钥指纹>）。[fingerprint] 是完整指纹（sha256//…）或前 128 位（sha256-128//…），
 * 见 [PinnedTrustManager]。
 */
data class PairingUri(val address: String, val domain: String, val fingerprint: String, val code: String) {
    companion object {
        fun parse(text: String): PairingUri? {
            val uri = runCatching { Uri.parse(text.trim()) }.getOrNull() ?: return null
            if (uri.scheme != "nagi-app" || uri.host != "pair") return null
            val fp = when (uri.getQueryParameter("v")) {
                "1" -> uri.getQueryParameter("fp").orEmpty().takeIf { it.startsWith(PIN_FULL) }
                "2" -> uri.getQueryParameter("k").orEmpty().takeIf { it.length == 22 }?.let { PIN_128 + it }
                else -> null
            } ?: return null
            val a = uri.getQueryParameter("a").orEmpty()
            val c = uri.getQueryParameter("c").orEmpty()
            if (a.isEmpty() || c.isEmpty()) return null
            return PairingUri(a, uri.getQueryParameter("d").orEmpty(), fp, c)
        }
    }
}
