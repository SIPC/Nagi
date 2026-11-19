package ink.sipc.nagi.pairing

import ink.sipc.nagi.ui.rowClickable
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.shadow
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.repeatOnLifecycle
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.AppApproval
import ink.sipc.nagi.security.AuthCancelledException
import ink.sipc.nagi.security.Biometric
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.NagiTheme
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

// 手机确认（见 docs/android.md#手机确认）：网页发起的高权限确认、登录的第二步，在这里用确认密钥签名确认。
// App 在前台时每 3 秒查一次等确认的请求，有就弹出对话框：写明是什么、从哪里发起的，照着网页输入两位数字，
// 点「确认」后弹系统验证框（指纹、面容或锁屏密码），签名里带着请求、种类和数字。

private const val POLL_MS = 3_000L

/** 放在主界面里：有等确认的请求时弹出对话框。关掉（返回键）的请求这次不再弹。 */
@Composable
fun ApprovalPrompts(session: PanelSession) {
    val owner = LocalLifecycleOwner.current
    var pending by remember { mutableStateOf<List<AppApproval>>(emptyList()) }
    var ignored by remember { mutableStateOf(setOf<String>()) }
    LaunchedEffect(session, owner) {
        owner.lifecycle.repeatOnLifecycle(Lifecycle.State.RESUMED) {
            while (true) {
                // 查不到（没有网、旧版面板没有这个接口）就当作没有，下一轮再查。
                pending = runCatching { session.approvals() }.getOrDefault(emptyList())
                delay(POLL_MS)
            }
        }
    }
    val waiting = pending.filter { it.id !in ignored }
    if (waiting.isEmpty()) return
    // 同时有几个请求（不同的来源各发起了一个）时先列出来让成员选自己发起的那个，不直接弹最早的：
    // 别处知道密码的人先发一个，成员照自己屏幕上的数字输进去，输的就是别人的请求（2026-11-19-402545d#3）。
    var chosen by remember { mutableStateOf<String?>(null) }
    val current = waiting.firstOrNull { it.id == chosen } ?: waiting.singleOrNull()
    if (current == null) {
        ApprovalChooser(session, waiting, onPick = { chosen = it.id }, onIgnore = { ignored = ignored + waiting.map { it.id } }, onDenied = { ids -> pending = pending.filter { it.id !in ids } })
        return
    }
    ApprovalDialog(
        session, current,
        onDone = { pending = pending.filter { it.id != current.id }; chosen = null },
        onIgnore = { ignored = ignored + current.id; chosen = null },
    )
}

/** 几个请求同时在等：写明各自的种类、来源和发起时间，点一个进去确认；「全部拒绝」把它们都拒绝掉。 */
@Composable
private fun ApprovalChooser(session: PanelSession, list: List<AppApproval>, onPick: (AppApproval) -> Unit, onIgnore: () -> Unit, onDenied: (List<String>) -> Unit) {
    val scope = rememberCoroutineScope()
    var busy by remember { mutableStateOf(false) }
    val fmt = remember { java.text.SimpleDateFormat("HH:mm:ss", java.util.Locale.getDefault()) }
    Dialog(
        onDismissRequest = { if (!busy) onIgnore() },
        properties = DialogProperties(usePlatformDefaultWidth = false, dismissOnClickOutside = false),
    ) {
        val shape = RoundedCornerShape(12.dp)
        val c = NagiTheme.colors
        Column(
            Modifier.padding(horizontal = 24.dp).widthIn(max = 420.dp).fillMaxWidth().shadow(16.dp, shape).background(c.card, shape).padding(20.dp),
        ) {
            Text(stringResource(R.string.approval_many_title, list.size), style = NagiTheme.type.section, color = c.foreground)
            Spacer(Modifier.height(8.dp))
            Text(stringResource(R.string.approval_many_text), style = NagiTheme.type.body, color = c.mutedForeground)
            Spacer(Modifier.height(8.dp))
            list.forEach { a ->
                Column(Modifier.fillMaxWidth().rowClickable({ if (!busy) onPick(a) }).padding(vertical = 10.dp)) {
                    Text(
                        stringResource(if (a.kind == "login") R.string.approval_login_title else R.string.approval_elevate_title) + " · " +
                            fmt.format(java.util.Date(a.createdAt.seconds * 1000)),
                        style = NagiTheme.type.body, color = c.foreground,
                    )
                    Text(listOf(a.ip, a.agent).filter { it.isNotEmpty() }.joinToString(" · "), style = NagiTheme.type.mono, color = c.mutedForeground)
                }
            }
            Spacer(Modifier.height(12.dp))
            NagiButton(
                stringResource(R.string.approval_deny_all),
                {
                    busy = true
                    scope.launch {
                        val done = list.filter { runCatching { session.denyApproval(it.id) }.isSuccess }.map { it.id }
                        busy = false
                        onDenied(done)
                    }
                },
                Modifier.fillMaxWidth(), variant = ButtonVariant.Outline, loading = busy,
            )
        }
    }
}

@Composable
private fun ApprovalDialog(session: PanelSession, a: AppApproval, onDone: () -> Unit, onIgnore: () -> Unit) {
    val activity = LocalContext.current as FragmentActivity
    val scope = rememberCoroutineScope()
    var code by remember(a.id) { mutableStateOf("") }
    var busy by remember(a.id) { mutableStateOf<String?>(null) }
    var error by remember(a.id) { mutableStateOf<String?>(null) }
    val login = a.kind == "login"
    val title = stringResource(if (login) R.string.approval_login_title else R.string.approval_elevate_title)
    val cancel = stringResource(R.string.cancel)
    fun run(key: String, block: suspend () -> Unit) {
        if (busy != null) return
        busy = key
        error = null
        scope.launch {
            try {
                block()
                onDone()
            } catch (e: AuthCancelledException) {
                // 留在对话框里，可以再点。
            } catch (e: Exception) {
                error = e.message
                code = ""
            } finally {
                busy = null
            }
        }
    }
    Dialog(
        onDismissRequest = { if (busy == null) onIgnore() },
        properties = DialogProperties(usePlatformDefaultWidth = false, dismissOnClickOutside = false),
    ) {
        val shape = RoundedCornerShape(12.dp)
        val c = NagiTheme.colors
        Column(
            Modifier.padding(horizontal = 24.dp).widthIn(max = 420.dp).fillMaxWidth().shadow(16.dp, shape).background(c.card, shape).padding(20.dp),
        ) {
            Text(title, style = NagiTheme.type.section, color = c.foreground)
            Spacer(Modifier.height(8.dp))
            Text(
                stringResource(if (login) R.string.approval_login_text else R.string.approval_elevate_text, session.panel.title),
                style = NagiTheme.type.body, color = c.mutedForeground,
            )
            Spacer(Modifier.height(8.dp))
            Text(listOf(a.ip, a.agent).filter { it.isNotEmpty() }.joinToString(" · "), style = NagiTheme.type.mono, color = c.foreground)
            Spacer(Modifier.height(4.dp))
            Text(stringResource(R.string.approval_account, session.panel.displayName.ifEmpty { session.panel.username }), style = NagiTheme.type.secondary, color = c.mutedForeground)
            Text(stringResource(R.string.approval_not_you), style = NagiTheme.type.secondary, color = c.mutedForeground)
            Spacer(Modifier.height(16.dp))
            NagiTextField(
                code, { v -> code = v.filter { it.isDigit() }.take(2) }, label = stringResource(R.string.approval_code),
                placeholder = stringResource(R.string.approval_code_hint), keyboardType = KeyboardType.Number, enabled = busy == null,
            )
            error?.let {
                Spacer(Modifier.height(8.dp))
                Text(it, style = NagiTheme.type.body, color = c.destructive)
            }
            Spacer(Modifier.height(20.dp))
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                NagiButton(
                    stringResource(R.string.approval_deny), { run("deny") { session.denyApproval(a.id) } }, Modifier.weight(1f),
                    variant = ButtonVariant.Outline, loading = busy == "deny", enabled = busy == null,
                )
                NagiButton(
                    stringResource(R.string.approval_confirm),
                    {
                        run("approve") {
                            session.approve(a, code) { msg ->
                                val sig = Biometric.authenticate(activity, session.confirmSignature(), title, null, cancel)
                                sig.update(msg)
                                sig.sign()
                            }
                        }
                    },
                    Modifier.weight(1f), loading = busy == "approve", enabled = busy == null && code.length == 2,
                )
            }
        }
    }
}

/**
 * App 自己的高权限确认（见 docs/android.md#高权限确认和重新验证）：弹系统验证框，用确认密钥签名面板的挑战。
 * 返回 true 表示确认过了，可以重试刚才的操作；取消时返回 false。其他错误照常抛出。
 */
@Composable
fun rememberElevate(session: PanelSession): suspend () -> Boolean {
    val activity = LocalContext.current as FragmentActivity
    val title = stringResource(R.string.prompt_elevate_title)
    val cancel = stringResource(R.string.cancel)
    return remember(session, activity) {
        suspend {
            try {
                session.elevate { msg ->
                    val sig = Biometric.authenticate(activity, session.confirmSignature(), title, null, cancel)
                    sig.update(msg)
                    sig.sign()
                }
                true
            } catch (e: AuthCancelledException) {
                false
            }
        }
    }
}
