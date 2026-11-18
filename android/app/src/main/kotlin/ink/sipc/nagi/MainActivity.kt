package ink.sipc.nagi

import android.content.Intent
import android.os.Bundle
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

// BiometricPrompt 要 FragmentActivity。
class MainActivity : FragmentActivity() {
    /** 从链接（系统相机扫码）打开的配对二维码。 */
    private val pendingPair = MutableStateFlow<PairingUri?>(null)

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        handle(intent)
        val app = application as NagiApplication
        setContent {
            NagiTheme {
                AppRoot(app.graph, app.lock, pendingPair)
            }
        }
        keepAlive(app)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handle(intent)
    }

    private fun handle(intent: Intent?) {
        intent?.dataString?.let(PairingUri::parse)?.let { pendingPair.value = it }
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
