package ink.sipc.nagi.settings

import ink.sipc.nagi.ui.LocalElevate
import ink.sipc.nagi.ui.SettingsRow
import ink.sipc.nagi.ui.ChoiceRow
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.Column
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.Dimens
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.Spacer
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
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.network.ElevationRequiredException
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SkeletonRows
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch

// 设置子页共用的：查询（加载、出错、重试）和操作（转圈、出错）。规则同网页的状态表：加载用骨架，失败写红字和「重试」。

/** 一次查询的状态。[value] 有了以后再刷新时保留旧的（不闪回骨架）。 */
@Stable
internal class LoadState<T>(private val scope: CoroutineScope, private val load: suspend () -> T) {
    var value by mutableStateOf<T?>(null)
    var error by mutableStateOf<String?>(null)
        private set
    var loading by mutableStateOf(false)
        private set

    fun reload() {
        loading = true
        error = null
        scope.launch {
            try {
                value = load()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                error = e.message.orEmpty()
            } finally {
                loading = false
            }
        }
    }
}

/** 进入页面时查一次。 */
@Composable
internal fun <T> rememberLoad(load: suspend () -> T): LoadState<T> {
    val scope = rememberCoroutineScope()
    return remember { LoadState(scope, load).also { it.reload() } }
}

/** 有数据时显示内容；还没有时显示骨架或者出错和「重试」。 */
@Composable
internal fun <T> Loaded(state: LoadState<T>, rows: Int = 3, content: @Composable (T) -> Unit) {
    val v = state.value
    when {
        v != null -> content(v)
        state.error != null -> InlineError(state.error.orEmpty(), stringResource(R.string.retry), state::reload, Modifier.padding(vertical = 8.dp))
        else -> SkeletonRows(rows, Modifier.padding(top = 8.dp))
    }
}

/** 页面上的操作：同一时间只做一个，[busy] 是正在做的那个（按钮转圈），[error] 是上一次失败的原因。 */
@Stable
internal class Action(private val scope: CoroutineScope, private val elevationText: String, private val elevate: (suspend () -> Boolean)?) {
    var busy by mutableStateOf<String?>(null)
        private set
    var error by mutableStateOf<String?>(null)

    /** [passwordOnly]：面板只认密码或验证码做的高权限确认的操作，手机上的确认不算，不弹系统验证框，直接提示到网页上做。 */
    fun run(key: String, passwordOnly: Boolean = false, block: suspend () -> Unit) {
        if (busy != null) return
        busy = key
        error = null
        scope.launch {
            try {
                block()
            } catch (e: CancellationException) {
                throw e
            } catch (e: ElevationRequiredException) {
                // 要先做高权限确认：弹系统验证框，确认后再做一次。确认了面板还是要求验证的，是只认密码或验证码的操作
                // （给自己留登录凭据的），请到网页上做。
                try {
                    if (elevate == null || passwordOnly) {
                        error = elevationText
                    } else if (elevate.invoke()) {
                        block()
                    }
                } catch (e2: CancellationException) {
                    throw e2
                } catch (e2: ElevationRequiredException) {
                    error = elevationText
                } catch (e2: Exception) {
                    error = e2.message.orEmpty()
                }
            } catch (e: Exception) {
                error = e.message.orEmpty()
            } finally {
                busy = null
            }
        }
    }
}

@Composable
internal fun rememberAction(): Action {
    val scope = rememberCoroutineScope()
    val text = stringResource(R.string.needs_web_elevation)
    val elevate = LocalElevate.current
    return remember(elevate) { Action(scope, text, elevate) }
}

/**
 * 添加和修改成员、添加备用面板、提升为主面板在这个面板上是不是只能到网页上做（见 [ink.sipc.nagi.data.Access.webOnlyChanges]）。
 * 还没查到时为 null；查不到时按能做算，入口照常显示，面板拒绝时再提示。
 */
@Composable
internal fun rememberWebOnly(session: PanelSession): Boolean? {
    val access = rememberLoad { session.access() }
    return access.value?.webOnlyChanges ?: if (access.error != null) false else null
}

/** 操作失败的原因：红字，放在页面或对话框里出错的那一块上面。 */
@Composable
internal fun ActionError(action: Action, modifier: Modifier = Modifier) {
    val e = action.error ?: return
    Text(e, style = NagiTheme.type.body, color = NagiTheme.colors.destructive, modifier = modifier.padding(vertical = 8.dp))
}

/** 距现在多久：刚刚、4 分钟前、2 小时前、3 天前；超过 30 天写日期。 */
@Composable
internal fun ago(epochSeconds: Long): String {
    val d = (System.currentTimeMillis() / 1000 - epochSeconds).coerceAtLeast(0)
    return when {
        d < 60 -> stringResource(R.string.just_now)
        d < 3600 -> stringResource(R.string.minutes_ago, d / 60)
        d < 86400 -> stringResource(R.string.hours_ago, d / 3600)
        d < 30 * 86400 -> stringResource(R.string.days_ago, d / 86400)
        else -> date(epochSeconds)
    }
}

/** 日期：2026-11-18。 */
internal fun date(epochSeconds: Long): String =
    java.text.SimpleDateFormat("yyyy-MM-dd", java.util.Locale.getDefault()).format(java.util.Date(epochSeconds * 1000))

/** 日期和时间：2026-11-18 21:30。 */
internal fun dateTime(epochSeconds: Long): String =
    java.text.SimpleDateFormat("yyyy-MM-dd HH:mm", java.util.Locale.getDefault()).format(java.util.Date(epochSeconds * 1000))

/**
 * 改一项文字：输入框、说明和保存按钮（占满宽度）。保存失败时在输入框下面写原因（面板给的说明），成功后 [onDone]。
 * 保存的是去掉首尾空白的值。
 */
@Composable
internal fun EditField(
    initial: String,
    placeholder: String,
    help: String,
    onDone: () -> Unit,
    label: String? = null,
    maxLength: Int = 255,
    saveLabel: String? = null,
    save: suspend (String) -> Unit,
) {
    var value by remember { mutableStateOf(initial) }
    val action = rememberAction()
    val submit = { action.run("save") { save(value.trim()); onDone() } }
    Spacer(Modifier.height(Dimens.topBarToContent + 8.dp))
    NagiTextField(value, { value = it; action.error = null }, label = label, placeholder = placeholder, error = action.error != null, maxLength = maxLength, onDone = { submit() })
    ActionError(action)
    Spacer(Modifier.height(if (action.error == null) 8.dp else 0.dp))
    Text(help, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
    Spacer(Modifier.height(20.dp))
    NagiButton(saveLabel ?: stringResource(R.string.save), { submit() }, Modifier.fillMaxWidth(), loading = action.busy == "save")
}

/**
 * 选择一项（网页的 Select）：一行写名称、说明和当前的值，点了从底部弹出选项，选中马上保存（[onSelect]）。
 * [busy] 时值的位置转圈。
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun <T> SelectRow(
    title: String,
    options: List<T>,
    selected: T,
    label: @Composable (T) -> String,
    onSelect: (T) -> Unit,
    description: String? = null,
    enabled: Boolean = true,
) {
    var open by remember { mutableStateOf(false) }
    Column {
        SettingsRow(title, { if (enabled) open = true }, value = label(selected))
        if (!description.isNullOrEmpty()) {
            Text(description, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(bottom = 6.dp))
        }
    }
    if (open) {
        ModalBottomSheet(onDismissRequest = { open = false }, containerColor = NagiTheme.colors.card) {
            Column(Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 12.dp)) {
                Text(title, style = NagiTheme.type.section, color = NagiTheme.colors.foreground, modifier = Modifier.padding(bottom = 8.dp))
                options.forEach { o ->
                    ChoiceRow(label(o), o == selected, {
                        open = false
                        if (o != selected) onSelect(o)
                    })
                }
            }
        }
    }
}
