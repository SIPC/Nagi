package ink.sipc.nagi

import ink.sipc.nagi.settings.AppLocales
import ink.sipc.nagi.data.ThemeMode
import androidx.compose.runtime.getValue
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.activity.SystemBarStyle
import android.graphics.Color
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.Bundle
import android.view.WindowManager
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import ink.sipc.nagi.network.DeviceRemovedException
import ink.sipc.nagi.network.PairingUri
import ink.sipc.nagi.network.UnlockRequiredException
import ink.sipc.nagi.ui.NagiTheme
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.launch

/** 外部的配对链接收不收：从最近任务重新打开时系统再给的最初的 intent 不算，已经添加了面板时不收（v0.34.0-b L4）。 */
internal fun acceptsPairLink(flags: Int, hasPanel: Boolean): Boolean =
    flags and Intent.FLAG_ACTIVITY_LAUNCHED_FROM_HISTORY == 0 && !hasPanel

/** 添加流程停在哪一页时收外部链接：只在欢迎页（还没进导航时也算）；扫码页、确认页打开期间不收。 */
internal fun acceptsPairLinkOn(route: String?): Boolean = route == null || route == "welcome"

// BiometricPrompt 要 FragmentActivity。
class MainActivity : FragmentActivity() {
    /** 从链接（系统相机扫码）打开的配对二维码。只在还没有添加面板时接收（见 [handle]）。 */
    private val pendingPair = MutableStateFlow<PairingUri?>(null)

    // Android 13 以前按本机设置换掉界面语言（见 AppLocales）。
    override fun attachBaseContext(newBase: Context) {
        super.attachBaseContext(AppLocales.wrap(newBase))
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        // 最近任务里不留首页的截图：超过解锁时长后，拿到手机的人打开最近任务能看到上次离开时的告警、节点、流量，
        // 解锁页挡的就是这些（安全审查 v0.34.0-b L5）。Android 13 起系统直接不截；更早的版本见 onPause。
        if (Build.VERSION.SDK_INT >= 33) setRecentsScreenshotEnabled(false)
        // 重建（切换语言、旋转、进程被回收后恢复）时拿到的还是最初的 intent，不再处理，旧链接不会再打开一次确认页。
        if (savedInstanceState == null) handle(intent)
        val app = application as NagiApplication
        setContent {
            // 外观：跟随系统，或者本机设置里固定的浅色、深色。状态栏和导航栏的图标颜色跟着变。
            val mode by app.graph.prefs.theme.collectAsState()
            val dark = when (mode) {
                ThemeMode.System -> isSystemInDarkTheme()
                ThemeMode.Light -> false
                ThemeMode.Dark -> true
            }
            LaunchedEffect(dark) {
                val bars = if (dark) SystemBarStyle.dark(Color.TRANSPARENT) else SystemBarStyle.light(Color.TRANSPARENT, Color.TRANSPARENT)
                enableEdgeToEdge(statusBarStyle = bars, navigationBarStyle = bars)
            }
            NagiTheme(darkTheme = dark) {
                AppRoot(app.graph, app.lock, pendingPair)
            }
        }
        keepAlive(app)
    }

    /** onPause 时为最近任务加上的 FLAG_SECURE（页面自己设的不算），onResume 时只去掉这个。 */
    private var recentsSecure = false

    // Android 13 以前没有 setRecentsScreenshotEnabled：离开前台时加上 FLAG_SECURE，最近任务里的截图是空白，回来再去掉，
    // 平时照常能截屏。
    override fun onPause() {
        val secure = WindowManager.LayoutParams.FLAG_SECURE
        if (Build.VERSION.SDK_INT < 33 && window.attributes.flags and secure == 0) {
            window.addFlags(secure)
            recentsSecure = true
        }
        super.onPause()
    }

    override fun onResume() {
        super.onResume()
        if (recentsSecure) {
            window.clearFlags(WindowManager.LayoutParams.FLAG_SECURE)
            recentsSecure = false
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handle(intent)
    }

    /**
     * 外部链接打开的配对二维码（安全审查 v0.34.0-b L4）：处理完清掉 intent 的链接；从最近任务重新打开时系统再给的
     * 最初的 intent 不算；已经添加了面板时不接收，外部链接不能把 App 换到别的面板。确认页打开期间来的链接在 AppRoot 里不理。
     */
    private fun handle(intent: Intent?) {
        if (intent == null) return
        val data = intent.dataString ?: return
        intent.data = null
        if (!acceptsPairLink(intent.flags, hasPanel = (application as NagiApplication).graph.session.value != null)) return
        PairingUri.parse(data)?.let { pendingPair.value = it }
    }

    /**
     * 在前台、已解锁时每 4 分钟用设备密钥换一次令牌，面板同时把解锁时间往后延，连续使用时不会反复要求验证
     * （见 docs/android.md#解锁-app）。在后台不换取。
     */
    private fun keepAlive(app: NagiApplication) {
        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.STARTED) {
                while (true) {
                    delay(4 * 60_000L)
                    val session = app.graph.session.value ?: continue
                    if (app.lock.locked.value != null) continue
                    try {
                        session.tokens.invalidate()
                        session.tokens.token()
                    } catch (e: UnlockRequiredException) {
                        app.lock.lockNow()
                    } catch (e: DeviceRemovedException) {
                        app.graph.forget()
                    } catch (e: Exception) {
                        // 连不上面板：下一次请求再换。
                    }
                }
            }
        }
    }
}
