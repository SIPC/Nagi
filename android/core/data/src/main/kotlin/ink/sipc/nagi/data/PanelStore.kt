package ink.sipc.nagi.data

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
    /** 本机给面板起的显示名称（只在这台手机上），为空时显示面板名称 [name]。 */
    val alias: String = "",
) {
    /** 显示用的名称：本机的显示名称优先，没有时是面板名称。 */
    val title: String get() = alias.ifBlank { name }
}

class PanelStore(context: Context) {
    private val prefs = context.getSharedPreferences("panels", Context.MODE_PRIVATE)

    fun current(): Panel? {
        val raw = prefs.getString(KEY, null) ?: return null
        return runCatching {
            val o = JSONObject(raw)
            Panel(
                o.getString("id"), o.getString("name"), o.getString("address"), o.optString("domain"), o.getString("fingerprint"),
                o.getString("deviceId"), o.optString("username"), o.optString("displayName"), o.optString("alias"),
            )
        }.getOrNull()
    }

    fun save(p: Panel) {
        val o = JSONObject()
            .put("id", p.id).put("name", p.name).put("address", p.address).put("domain", p.domain).put("fingerprint", p.fingerprint)
            .put("deviceId", p.deviceId).put("username", p.username).put("displayName", p.displayName).put("alias", p.alias)
        prefs.edit().putString(KEY, o.toString()).apply()
    }

    fun clear() {
        prefs.edit().remove(KEY).apply()
    }

    private companion object {
        const val KEY = "current"
    }
}
