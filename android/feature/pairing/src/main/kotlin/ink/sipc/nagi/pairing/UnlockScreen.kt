package ink.sipc.nagi.pairing

import androidx.compose.runtime.setValue
import androidx.compose.runtime.getValue
import ink.sipc.nagi.ui.IconCircle
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.fragment.app.FragmentActivity
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.network.DeviceRemovedException
import ink.sipc.nagi.security.AuthCancelledException
import ink.sipc.nagi.security.Biometric
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTheme
import kotlinx.coroutines.launch

/**
 * 解锁 App（s5 第 4 张）：打开 App、或者离开前台超过解锁时长后回来时，用确认密钥换取令牌（见 docs/android.md#解锁-app）。
 * 进入时自动弹出系统验证框。
 */
@Composable
fun UnlockScreen(session: PanelSession, idleMinutes: Int?, onUnlocked: () -> Unit, onRemoved: () -> Unit) {
    val activity = LocalContext.current as FragmentActivity
    val scope = rememberCoroutineScope()
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val title = stringResource(R.string.prompt_unlock_title, session.panel.title)
    val cancel = stringResource(R.string.cancel)
    fun unlock() {
        if (busy) return
        busy = true
        error = null
        scope.launch {
            try {
                session.tokens.unlock { msg ->
                    val sig = Biometric.authenticate(activity, session.confirmSignature(), title, null, cancel)
                    sig.update(msg)
                    sig.sign()
                }
                onUnlocked()
            } catch (e: AuthCancelledException) {
                // 留在这一页，可以再点「解锁」。
            } catch (e: DeviceRemovedException) {
                onRemoved()
            } catch (e: Exception) {
                error = e.message
            } finally {
                busy = false
            }
        }
    }
    LaunchedEffect(Unit) { unlock() }
    Box(Modifier.fillMaxSize().padding(horizontal = Dimens.pageHorizontal), contentAlignment = Alignment.Center) {
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            IconCircle(NagiIcons.Lock)
            Spacer(Modifier.height(16.dp))
            Text(session.panel.title, style = NagiTheme.type.topBar, color = NagiTheme.colors.foreground)
            Spacer(Modifier.height(4.dp))
            Text(
                if (idleMinutes != null) stringResource(R.string.unlock_idle, idleMinutes) else stringResource(R.string.unlock_locked),
                style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground,
            )
            Spacer(Modifier.height(20.dp))
            NagiButton(stringResource(R.string.unlock), ::unlock, Modifier.widthIn(min = 120.dp), loading = busy)
            if (error != null) {
                Spacer(Modifier.height(12.dp))
                Text(error.orEmpty(), style = NagiTheme.type.body, color = NagiTheme.colors.destructive, textAlign = TextAlign.Center)
            }
        }
    }
}
