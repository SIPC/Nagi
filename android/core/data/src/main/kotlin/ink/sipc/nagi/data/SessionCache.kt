package ink.sipc.nagi.data

import android.content.Context
import android.util.Base64
import ink.sipc.nagi.proto.v1.Overview
import ink.sipc.nagi.security.CacheCipher

// 本机缓存（见 docs/android.md#缓存）：打开 App 时先显示上次的首页数据，同时连接面板，连上以后由订阅流覆盖。
// 只缓存首页的状态类数据（需要处理、关键数字、流量排行），按面板分开、用 Keystore 里的密钥加密。
// 另外记下面板给的下一次换取用的挑战（15 分钟内有效），下次打开少一次往返；挑战不是凭据，没有设备密钥用不了。

class SessionCache(context: Context, private val cipher: CacheCipher) {
    private val prefs = context.applicationContext.getSharedPreferences("cache", Context.MODE_PRIVATE)

    /** 上次的首页数据和它的时间；没有或解不开时返回 null。 */
    fun overview(panelId: String): Pair<Overview, Long>? {
        val raw = prefs.getString("overview.$panelId", null) ?: return null
        val at = prefs.getLong("overview.$panelId.at", 0)
        val plain = cipher.decrypt(panelId, Base64.decode(raw, Base64.NO_WRAP)) ?: return null
        return runCatching { Overview.parseFrom(plain) to at }.getOrNull()
    }

    fun saveOverview(panelId: String, o: Overview, at: Long) {
        val enc = cipher.encrypt(panelId, o.toByteArray())
        prefs.edit()
            .putString("overview.$panelId", Base64.encodeToString(enc, Base64.NO_WRAP))
            .putLong("overview.$panelId.at", at)
            .apply()
    }

    /** 记下的下一次换取用的挑战和拿到它的时间。 */
    fun challenge(panelId: String): Pair<String, Long>? {
        val c = prefs.getString("challenge.$panelId", null) ?: return null
        return c to prefs.getLong("challenge.$panelId.at", 0)
    }

    fun saveChallenge(panelId: String, challenge: String?, at: Long) {
        val e = prefs.edit()
        if (challenge == null) e.remove("challenge.$panelId").remove("challenge.$panelId.at")
        else e.putString("challenge.$panelId", challenge).putLong("challenge.$panelId.at", at)
        e.apply()
    }

    /** 退出面板时删除这个面板的缓存和密钥。 */
    fun clear(panelId: String) {
        prefs.edit().apply { prefs.all.keys.filter { it.contains(".$panelId") }.forEach { remove(it) } }.apply()
        cipher.delete(panelId)
    }
}
