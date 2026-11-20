package ink.sipc.nagi.settings

import androidx.compose.foundation.layout.padding
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.width
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import com.google.protobuf.ByteString
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.AccessToken
import ink.sipc.nagi.proto.v1.ChangePasswordRequest
import ink.sipc.nagi.proto.v1.DeletePasskeyRequest
import ink.sipc.nagi.proto.v1.DisableTOTPRequest
import ink.sipc.nagi.proto.v1.GetSecurityRequest
import ink.sipc.nagi.proto.v1.ListAccessTokensRequest
import ink.sipc.nagi.proto.v1.ListSessionsRequest
import ink.sipc.nagi.proto.v1.LoginSession
import ink.sipc.nagi.proto.v1.MeRequest
import ink.sipc.nagi.proto.v1.Passkey
import ink.sipc.nagi.proto.v1.RevokeAccessTokenRequest
import ink.sipc.nagi.proto.v1.RevokeSessionRequest
import ink.sipc.nagi.proto.v1.UpdateProfileRequest
import ink.sipc.nagi.ui.ActionRow
import ink.sipc.nagi.ui.Avatar
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SettingsGroup
import java.io.ByteArrayOutputStream
import kotlin.math.min

// 我的账号：个人资料、账号安全（和网页「设置 → 我的账号」一样，见 web/src/components/account.tsx、pages/security.tsx）。

/** 头像裁成正方形、缩到 128（和网页一样），存成 WebP。 */
private fun avatarBytes(context: android.content.Context, uri: Uri): ByteArray? = runCatching {
    val src = context.contentResolver.openInputStream(uri)?.use { BitmapFactory.decodeStream(it) } ?: return null
    val side = min(src.width, src.height)
    val square = Bitmap.createBitmap(src, (src.width - side) / 2, (src.height - side) / 2, side, side)
    val small = Bitmap.createScaledBitmap(square, 128, 128, true)
    val out = ByteArrayOutputStream()
    @Suppress("DEPRECATION")
    val format = if (Build.VERSION.SDK_INT >= 30) Bitmap.CompressFormat.WEBP_LOSSY else Bitmap.CompressFormat.WEBP
    small.compress(format, 90, out)
    out.toByteArray()
}.getOrNull()

/** 个人资料：头像（选图片、移除）和显示名称，保存时整份覆盖（UpdateProfile）。 */
@Composable
fun ProfileScreen(session: PanelSession, onBack: () -> Unit, onSaved: () -> Unit) {
    val me = rememberLoad { session.rpc { c, h -> c.auth.me(MeRequest.getDefaultInstance(), h) } }
    val action = rememberAction()
    SubPage(stringResource(R.string.profile), onBack) {
        Loaded(me) { m ->
            val context = LocalContext.current
            var name by remember(m) { mutableStateOf(m.displayName) }
            var avatar by remember(m) { mutableStateOf(m.avatar.toByteArray()) }
            val pick = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri ->
                if (uri != null) avatarBytes(context, uri)?.let { avatar = it }
            }
            Spacer(Modifier.height(Dimens.topBarToContent + 8.dp))
            Row(verticalAlignment = Alignment.CenterVertically) {
                Avatar(name.ifEmpty { m.username }, avatar, size = 56.dp)
                Spacer(Modifier.width(16.dp))
                // 并排按钮之间 12（docs/android.md 视觉「间距」）。
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    NagiButton(stringResource(R.string.avatar_pick), {
                        pick.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly))
                    }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    NagiButton(stringResource(R.string.avatar_remove), { avatar = ByteArray(0) }, variant = ButtonVariant.Ghost, size = ButtonSize.Small, enabled = avatar.isNotEmpty())
                }
            }
            Spacer(Modifier.height(4.dp))
            Text(stringResource(R.string.avatar_hint), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            Spacer(Modifier.height(20.dp))
            Text(stringResource(R.string.display_name), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            Spacer(Modifier.height(6.dp))
            NagiTextField(name, { name = it }, label = stringResource(R.string.display_name), placeholder = m.username, maxLength = 32)
            Spacer(Modifier.height(4.dp))
            Text(stringResource(R.string.display_name_hint, m.username), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            ActionError(action)
            Spacer(Modifier.height(20.dp))
            NagiButton(stringResource(R.string.save), {
                action.run("save") {
                    session.rpc { c, h ->
                        c.auth.updateProfile(UpdateProfileRequest.newBuilder().setDisplayName(name.trim()).setAvatar(ByteString.copyFrom(avatar)).build(), h)
                    }
                    onSaved()
                    onBack()
                }
            }, Modifier.fillMaxWidth(), loading = action.busy == "save")
        }
    }
}

/**
 * 账号安全：登录密码、两步验证、通行密钥、登录设备、访问令牌。
 * 开启两步验证、添加通行密钥、生成访问令牌、添加手机只认密码或验证码做的高权限确认（见 docs/android.md#高权限确认和重新验证），
 * App 上不能做，写明在网页上操作。
 */
@Composable
fun SecurityScreen(session: PanelSession, onBack: () -> Unit, open: (String) -> Unit, onSignedOut: () -> Unit) {
    val sec = rememberLoad { session.rpc { c, h -> c.settings.getSecurity(GetSecurityRequest.getDefaultInstance(), h) } }
    val sessions = rememberLoad { session.rpc { c, h -> c.settings.listSessions(ListSessionsRequest.getDefaultInstance(), h) }.sessionsList }
    val tokens = rememberLoad { session.rpc { c, h -> c.settings.listAccessTokens(ListAccessTokensRequest.getDefaultInstance(), h) }.tokensList }
    val action = rememberAction()
    var disablingTotp by remember { mutableStateOf(false) }
    var removing by remember { mutableStateOf<Passkey?>(null) }
    var signingOut by remember { mutableStateOf<LoginSession?>(null) }
    var revoking by remember { mutableStateOf<AccessToken?>(null) }
    SubPage(stringResource(R.string.account_security), onBack) {
        ActionError(action)
        Loaded(sec) { s ->
            ActionRow(stringResource(R.string.password), description = stringResource(R.string.password_desc)) {
                NagiButton(stringResource(R.string.password_change), { open(SettingsRoutes.PASSWORD) }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
            }
            if (s.totpEnabled) {
                ActionRow(stringResource(R.string.totp), description = stringResource(R.string.on_status)) {
                    NagiButton(stringResource(R.string.turn_off), { disablingTotp = true }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                }
            } else {
                ActionRow(stringResource(R.string.totp), description = stringResource(R.string.totp_off_app))
            }
            SettingsGroup(stringResource(R.string.passkeys)) {
                if (s.passkeysCount == 0) Note(stringResource(R.string.passkeys_empty))
                s.passkeysList.forEach { p ->
                    val detail = listOfNotNull(
                        stringResource(R.string.added_at, date(p.createdAt.seconds)),
                        if (p.hasLastUsedAt() && p.lastUsedAt.seconds > 0) stringResource(R.string.last_used, ago(p.lastUsedAt.seconds)) else stringResource(R.string.never_used),
                        if (p.otherDomain) stringResource(R.string.passkey_other_domain) else null,
                    ).joinToString(" · ")
                    ActionRow(p.name, description = detail) {
                        NagiButton(stringResource(R.string.delete), { removing = p }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    }
                }
                Note(stringResource(R.string.passkeys_add_on_web))
            }
        }
        SettingsGroup(stringResource(R.string.sessions)) {
            Loaded(sessions) { list ->
                list.sortedWith(compareByDescending<LoginSession> { it.current }.thenByDescending { it.lastSeen.seconds }).forEach { ls ->
                    val app = ls.kind == "app"
                    val title = if (app) ls.deviceName.ifEmpty { ls.model } else ls.agent.ifEmpty { "—" }
                    val detail = listOfNotNull(
                        if (ls.current) stringResource(R.string.current) else null,
                        if (app) listOf(ls.androidVersion.takeIf { it.isNotEmpty() }?.let { "Android $it" }, ls.appVersion.takeIf { it.isNotEmpty() }?.let { "App $it" }).filterNotNull().joinToString(" · ").ifEmpty { null } else null,
                        ls.lastIp.ifEmpty { ls.ip }.ifEmpty { null },
                        if (ls.hasLastSeen() && ls.lastSeen.seconds > 0) ago(ls.lastSeen.seconds) else null,
                    ).joinToString(" · ")
                    ActionRow(title, description = detail) {
                        if (!ls.current) NagiButton(stringResource(R.string.sign_out), { signingOut = ls }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    }
                }
            }
        }
        SettingsGroup(stringResource(R.string.access_tokens)) {
            Loaded(tokens) { list ->
                if (list.isEmpty()) Note(stringResource(R.string.access_tokens_empty))
                list.forEach { t ->
                    val perms = listOfNotNull(
                        stringResource(if (t.allowWrite) R.string.token_write else R.string.token_read),
                        if (t.allowExec) stringResource(R.string.token_exec) else null,
                        if (t.allowNodeExec) stringResource(R.string.token_node_exec) else null,
                        if (t.allowPanel) stringResource(R.string.token_panel) else null,
                    ).joinToString("、")
                    val used = if (t.hasLastUsedAt() && t.lastUsedAt.seconds > 0) stringResource(R.string.last_used, ago(t.lastUsedAt.seconds)) else stringResource(R.string.never_used)
                    ActionRow(t.name, description = "$perms · $used") {
                        NagiButton(stringResource(R.string.revoke), { revoking = t }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    }
                }
                Note(stringResource(R.string.access_tokens_create_on_web))
            }
        }
    }

    if (disablingTotp) {
        var code by remember { mutableStateOf("") }
        NagiDialog(
            title = stringResource(R.string.totp_disable), text = stringResource(R.string.totp_disable_help),
            confirm = stringResource(R.string.turn_off), cancel = stringResource(R.string.cancel), danger = true,
            busy = action.busy == "totp", confirmEnabled = code.isNotBlank(),
            onDismiss = { disablingTotp = false },
            onConfirm = {
                action.run("totp") {
                    // 6 位数字当验证码，其他当密码（和网页一样，一个输入框两种都认）。
                    val req = if (code.trim().matches(Regex("\\d{6}"))) DisableTOTPRequest.newBuilder().setCode(code.trim()) else DisableTOTPRequest.newBuilder().setPassword(code)
                    sec.value = session.rpc { c, h -> c.settings.disableTOTP(req.build(), h) }
                    disablingTotp = false
                }
            },
        ) {
            Spacer(Modifier.height(12.dp))
            NagiTextField(code, { code = it }, label = stringResource(R.string.totp_disable_label), placeholder = stringResource(R.string.totp_disable_placeholder), password = true)
            ActionError(action)
        }
    }
    removing?.let { p ->
        NagiDialog(
            title = stringResource(R.string.passkey_remove_title), text = stringResource(R.string.passkey_remove_confirm, p.name),
            confirm = stringResource(R.string.delete), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "passkey",
            onDismiss = { removing = null },
            onConfirm = {
                action.run("passkey") {
                    sec.value = session.rpc { c, h -> c.settings.deletePasskey(DeletePasskeyRequest.newBuilder().setId(p.id).build(), h) }
                    removing = null
                }
            },
        )
    }
    signingOut?.let { ls ->
        NagiDialog(
            title = stringResource(R.string.session_sign_out_title), text = stringResource(R.string.session_sign_out_confirm),
            confirm = stringResource(R.string.sign_out), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "session",
            onDismiss = { signingOut = null },
            onConfirm = {
                action.run("session") {
                    session.rpc { c, h -> c.settings.revokeSession(RevokeSessionRequest.newBuilder().setId(ls.id).build(), h) }
                    signingOut = null
                    sessions.reload()
                }
            },
        )
    }
    revoking?.let { t ->
        NagiDialog(
            title = stringResource(R.string.token_revoke_title), text = stringResource(R.string.token_revoke_confirm, t.name),
            confirm = stringResource(R.string.revoke), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "token",
            onDismiss = { revoking = null },
            onConfirm = {
                action.run("token") {
                    session.rpc { c, h -> c.settings.revokeAccessToken(RevokeAccessTokenRequest.newBuilder().setId(t.id).build(), h) }
                    revoking = null
                    tokens.reload()
                }
            },
        )
    }
}

/** 更改密码：当前密码、新密码（至少 8 个字符）。成功后这个成员的其他登录和所有手机都会退出，这台也要重新添加。 */
@Composable
fun PasswordScreen(session: PanelSession, onBack: () -> Unit, onSignedOut: () -> Unit) {
    var current by remember { mutableStateOf("") }
    var next by remember { mutableStateOf("") }
    val action = rememberAction()
    SubPage(stringResource(R.string.password_change), onBack) {
        Spacer(Modifier.height(Dimens.topBarToContent + 8.dp))
        Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text(stringResource(R.string.password_current), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            NagiTextField(current, { current = it }, label = stringResource(R.string.password_current), password = true)
            Spacer(Modifier.height(10.dp))
            Text(stringResource(R.string.password_new), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            NagiTextField(next, { next = it }, label = stringResource(R.string.password_new), placeholder = stringResource(R.string.password_hint), password = true)
        }
        Spacer(Modifier.height(12.dp))
        Text(stringResource(R.string.password_phone_warning), style = NagiTheme.type.secondary, color = NagiTheme.colors.warningText)
        ActionError(action)
        Spacer(Modifier.height(20.dp))
        NagiButton(stringResource(R.string.password_change), {
            action.run("save") {
                session.rpc { c, h -> c.auth.changePassword(ChangePasswordRequest.newBuilder().setCurrent(current).setNewPassword(next).build(), h) }
                // 面板已经删掉了这台手机的设备记录。
                onSignedOut()
            }
        }, Modifier.fillMaxWidth(), loading = action.busy == "save", enabled = current.isNotEmpty() && next.length >= 8)
    }
}

/** 一行灰色的说明（空列表、要在网页上操作的事项）。 */
@Composable
internal fun Note(text: String) {
    Text(text, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.padding(vertical = 8.dp))
}
