package ink.sipc.nagi.network

import android.net.Uri

/**
 * 网页「添加手机」二维码的内容（见 docs/android.md#扫码）：
 * nagi-app://pair?v=1&a=<对外地址:面板端口>&d=<面板域名>&fp=<公钥指纹>&c=<配对码>
 */
data class PairingUri(val address: String, val domain: String, val fingerprint: String, val code: String) {
    companion object {
        fun parse(text: String): PairingUri? {
            val uri = runCatching { Uri.parse(text.trim()) }.getOrNull() ?: return null
            if (uri.scheme != "nagi-app" || uri.host != "pair" || uri.getQueryParameter("v") != "1") return null
            val a = uri.getQueryParameter("a").orEmpty()
            val fp = uri.getQueryParameter("fp").orEmpty()
            val c = uri.getQueryParameter("c").orEmpty()
            if (a.isEmpty() || !fp.startsWith("sha256//") || c.isEmpty()) return null
            return PairingUri(a, uri.getQueryParameter("d").orEmpty(), fp, c)
        }
    }
}
