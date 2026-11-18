package ink.sipc.nagi.data

import android.content.Context
import ink.sipc.nagi.security.AppKeys
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

// 依赖组装（见 docs/android.md#工程「依赖注入手写」）：整个 App 一份，在 Application 里创建。

class AppGraph(context: Context, appVersion: String) {
    val keys = AppKeys()
    val store = PanelStore(context.applicationContext)
    val pairing = Pairing(keys, store, appVersion)

    private val _session = MutableStateFlow(store.current()?.let { PanelSession(it, keys) })

    /** 当前面板的连接，没有添加面板时为 null。 */
    val session: StateFlow<PanelSession?> = _session

    /** 添加面板以后切换到它。 */
    fun use(panel: Panel) {
        _session.value = PanelSession(panel, keys)
    }

    /** 这台手机已退出（本机退出或面板说设备不在了）：删除密钥和面板，回到欢迎页。 */
    fun forget() {
        _session.value?.let { keys.delete(it.panel.id) }
        store.clear()
        _session.value = null
    }
}
