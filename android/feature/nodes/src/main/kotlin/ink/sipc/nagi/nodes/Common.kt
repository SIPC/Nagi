package ink.sipc.nagi.nodes

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.os.Build
import android.widget.Toast
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.Stable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import ink.sipc.nagi.data.USAGE_DANGER
import ink.sipc.nagi.data.USAGE_WARNING
import ink.sipc.nagi.network.ElevationRequiredException
import ink.sipc.nagi.network.UnreachableException
import ink.sipc.nagi.ui.LocalElevate
import ink.sipc.nagi.ui.NagiTheme
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch

// 节点页共用的：操作（转圈、出错、高权限确认）、复制和分享、数字的写法。

internal class Factory(private val create: () -> ViewModel) : ViewModelProvider.Factory {
    @Suppress("UNCHECKED_CAST")
    override fun <T : ViewModel> create(modelClass: Class<T>): T = create() as T
}

/**
 * 页面上的操作：同一时间只做一个，[busy] 是正在做的那个（按钮转圈），[error] 是上一次失败的原因。
 * 面板要求先做高权限确认时（删除镜像）弹系统验证框，确认后再做一次（和设置页的写法一样）。
 * 连不上面板时不排队，直接提示没有执行（docs/android.md「断网时的操作」）。
 */
@Stable
internal class Action(
    private val scope: CoroutineScope,
    private val elevationText: String,
    private val unreachableText: String,
    private val elevate: (suspend () -> Boolean)?,
) {
    var busy by mutableStateOf<String?>(null)
        private set
    var error by mutableStateOf<String?>(null)

    fun run(key: String, block: suspend () -> Unit) {
        if (busy != null) return
        busy = key
        error = null
        scope.launch {
            try {
                block()
            } catch (e: CancellationException) {
                throw e
            } catch (e: ElevationRequiredException) {
                try {
                    if (elevate == null) error = elevationText
                    else if (elevate.invoke()) block()
                } catch (e2: CancellationException) {
                    throw e2
                } catch (e2: ElevationRequiredException) {
                    error = elevationText
                } catch (e2: Exception) {
                    error = message(e2)
                }
            } catch (e: Exception) {
                error = message(e)
            } finally {
                busy = null
            }
        }
    }

    private fun message(e: Exception) = if (e is UnreachableException) unreachableText else e.message.orEmpty()
}

@Composable
internal fun rememberAction(): Action {
    val scope = rememberCoroutineScope()
    val elevation = stringResource(R.string.needs_web_elevation)
    val unreachable = stringResource(R.string.not_done_unreachable)
    val elevate = LocalElevate.current
    return remember(elevate) { Action(scope, elevation, unreachable, elevate) }
}

/** 操作失败的原因：红字。 */
@Composable
internal fun ActionError(action: Action, modifier: Modifier = Modifier) {
    val e = action.error ?: return
    Text(e, style = NagiTheme.type.body, color = NagiTheme.colors.destructive, modifier = modifier.padding(vertical = 8.dp))
}

/** 复制一段文字：轻振一下，Android 13 以前自己提示「已复制」（13 起系统会提示）。 */
@Composable
internal fun rememberCopy(): (String) -> Unit {
    val context = LocalContext.current
    val haptic = LocalHapticFeedback.current
    val copied = stringResource(R.string.copied)
    return remember(context) {
        { text ->
            context.getSystemService(ClipboardManager::class.java)?.setPrimaryClip(ClipData.newPlainText(null, text))
            haptic.performHapticFeedback(HapticFeedbackType.LongPress)
            if (Build.VERSION.SDK_INT < 33) Toast.makeText(context, copied, Toast.LENGTH_SHORT).show()
        }
    }
}

/** 用系统的分享面板分享一段文字。 */
internal fun share(context: Context, text: String, title: String) {
    val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text)
    context.startActivity(Intent.createChooser(send, title))
}

/** 百分比，和网页的短格式一样：不到 10 保留一位小数，否则取整；很小但不是 0 时写 <0.1%。[v] 是百分数（12.5 表示 12.5%）。 */
internal fun percentText(v: Double): String = when {
    !v.isFinite() || v <= 0 -> "0%"
    v < 0.05 -> "<0.1%"
    v < 10 -> "%.1f%%".format(v)
    else -> "%.0f%%".format(v)
}

/** 占用的颜色：平时灰色，75% 起琥珀，90% 起红（和网页的占用指示一样）。 */
@Composable
internal fun usageColor(fraction: Double, normal: Color = NagiTheme.colors.mutedForeground): Color = when {
    fraction >= USAGE_DANGER -> NagiTheme.colors.destructive
    fraction >= USAGE_WARNING -> NagiTheme.colors.warning
    else -> normal
}

/** 占用的文字颜色：到警告时用 warningText（琥珀色的字在白底上对比度不够）。 */
@Composable
internal fun usageTextColor(fraction: Double, normal: Color): Color = when {
    fraction >= USAGE_DANGER -> NagiTheme.colors.destructive
    fraction >= USAGE_WARNING -> NagiTheme.colors.warningText
    else -> normal
}

/** 距现在多久：4 分钟、2 小时、3 天（离线多久）。 */
@Composable
internal fun durationText(epochSeconds: Long): String {
    val d = (System.currentTimeMillis() / 1000 - epochSeconds).coerceAtLeast(0)
    return when {
        d < 3600 -> stringResource(R.string.minutes, (d / 60).coerceAtLeast(1))
        d < 86400 -> stringResource(R.string.hours, d / 3600)
        else -> stringResource(R.string.days, d / 86400)
    }
}

/** 距现在多久：刚刚、4 分钟前、2 小时前、3 天前；超过 30 天写日期。 */
@Composable
internal fun agoText(epochSeconds: Long): String {
    val d = (System.currentTimeMillis() / 1000 - epochSeconds).coerceAtLeast(0)
    return when {
        d < 60 -> stringResource(R.string.just_now)
        d < 3600 -> stringResource(R.string.minutes_ago, d / 60)
        d < 86400 -> stringResource(R.string.hours_ago, d / 3600)
        d < 30 * 86400 -> stringResource(R.string.days_ago, d / 86400)
        else -> java.text.SimpleDateFormat("yyyy-MM-dd", java.util.Locale.getDefault()).format(java.util.Date(epochSeconds * 1000))
    }
}
