package io.github.sipc.nagi.data

import android.content.Context
import org.json.JSONObject

/**
 * 添加过的面板（见 docs/android.md#范围「手机上存什么」）：名称、地址、公钥指纹、设备 ID。
 * 这一版只有一个面板。panelId 是本机给面板起的编号，密钥别名里用它。
 */
data class Panel(
    val id: String,
    val name: String,
    val address: String,
    val domain: String,
    val fingerprint: String,
    val deviceId: String,
    val username: String,
    val displayName: String,
)

class PanelStore(context: Context) {
    private val prefs = context.getSharedPreferences("panels", Context.MODE_PRIVATE)

    fun current(): Panel? {
        val raw = prefs.getString(KEY, null) ?: return null
        return runCatching {
            val o = JSONObject(raw)
            Panel(
                o.getString("id"), o.getString("name"), o.getString("address"), o.optString("domain"), o.getString("fingerprint"),
                o.getString("deviceId"), o.optString("username"), o.optString("displayName"),
            )
        }.getOrNull()
    }

    fun save(p: Panel) {
        val o = JSONObject()
            .put("id", p.id).put("name", p.name).put("address", p.address).put("domain", p.domain).put("fingerprint", p.fingerprint)
            .put("deviceId", p.deviceId).put("username", p.username).put("displayName", p.displayName)
        prefs.edit().putString(KEY, o.toString()).apply()
    }

    fun clear() {
        prefs.edit().remove(KEY).apply()
    }

    private companion object {
        const val KEY = "current"
    }
}
