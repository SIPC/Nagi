package ink.sipc.nagi.settings

import android.os.Build
import android.widget.Toast
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.height
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.GetSecurityRequest
import ink.sipc.nagi.proto.v1.GetSettingsRequest
import ink.sipc.nagi.proto.v1.PanelSettings
import ink.sipc.nagi.proto.v1.SetEntranceRequest
import ink.sipc.nagi.proto.v1.SetPanelDomainApiRequest
import ink.sipc.nagi.proto.v1.UpdateSettingsRequest
import ink.sipc.nagi.ui.ActionRow
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog

// 面板访问（管理员，和网页 pages/access.tsx 一样）：面板名称、对外地址、绑定域名、公钥指纹、主密钥、安全入口。

/** 小的「复制」按钮：复制、轻振，Android 13 以前另外提示「已复制」（之后系统自己会提示）。 */
@Composable
internal fun CopyButton(text: String) {
    val context = LocalContext.current
    val haptic = LocalHapticFeedback.current
    val copied = stringResource(R.string.copied)
    NagiButton(stringResource(R.string.copy), {
        copy(context, text)
        haptic.performHapticFeedback(HapticFeedbackType.LongPress)
        if (Build.VERSION.SDK_INT < 33) Toast.makeText(context, copied, Toast.LENGTH_SHORT).show()
    }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
}

@Composable
fun AccessScreen(session: PanelSession, onBack: () -> Unit, open: (String) -> Unit) {
    val settings = rememberLoad { session.rpc { c, h -> c.settings.getSettings(GetSettingsRequest.getDefaultInstance(), h) }.settings }
    val sec = rememberLoad { session.rpc { c, h -> c.settings.getSecurity(GetSecurityRequest.getDefaultInstance(), h) } }
    val action = rememberAction()
    val context = LocalContext.current
    var unbinding by remember { mutableStateOf(false) }
    var closingEntrance by remember { mutableStateOf(false) }
    var shownEntrance by remember { mutableStateOf<String?>(null) }
    // 打开登录页的地址：绑了域名用域名，否则是对外地址和端口。
    val origin = session.panel.domain.takeIf { it.isNotEmpty() }?.let { "https://$it" } ?: "https://${session.panel.address}"
    SubPage(stringResource(R.string.panel_access), onBack) {
        ActionError(action)
        Loaded(settings) { s: PanelSettings ->
            ActionRow(stringResource(R.string.panel_name), description = s.name.ifEmpty { s.defaultName }) {
                NagiButton(stringResource(R.string.edit), { open(SettingsRoutes.PANEL_NAME) }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
            }
            ActionRow(stringResource(R.string.public_address), description = "${s.publicAddress.ifEmpty { stringResource(R.string.not_set) }}:${s.port} · ${stringResource(R.string.public_address_usage)}") {
                NagiButton(stringResource(R.string.edit), { open(SettingsRoutes.ACCESS_ADDRESS) }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
            }
            ActionRow(
                stringResource(R.string.domain),
                description = if (s.domain.isNotEmpty()) stringResource(R.string.domain_bound, s.domain) else stringResource(R.string.domain_unbound),
            ) {
                if (s.domain.isNotEmpty()) {
                    NagiButton(stringResource(R.string.domain_unbind), { unbinding = true }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                }
                NagiButton(
                    stringResource(if (s.domain.isNotEmpty()) R.string.edit else R.string.domain_bind), { open(SettingsRoutes.ACCESS_DOMAIN) },
                    variant = ButtonVariant.Outline, size = ButtonSize.Small,
                )
            }
            ActionRow(stringResource(R.string.panel_fingerprint), description = s.fingerprint) { CopyButton(s.fingerprint) }
            if (s.masterKeyPath.isNotEmpty()) {
                ActionRow(stringResource(R.string.master_key), description = "${s.masterKeyPath} · ${stringResource(R.string.master_key_backup)}") { CopyButton(s.masterKeyPath) }
            }
        }
        Loaded(sec, rows = 1) { e ->
            val url = if (e.entrance.isNotEmpty()) "$origin/${e.entrance}" else ""
            ActionRow(
                stringResource(R.string.entrance),
                description = if (url.isNotEmpty()) "$url · ${stringResource(R.string.entrance_on)}" else stringResource(R.string.entrance_off),
            ) {
                if (url.isNotEmpty()) CopyButton(url)
                NagiButton(
                    stringResource(if (url.isNotEmpty()) R.string.turn_off else R.string.turn_on),
                    {
                        if (url.isNotEmpty()) {
                            closingEntrance = true
                        } else {
                            action.run("entrance") {
                                val next = session.rpc { c, h -> c.settings.setEntrance(SetEntranceRequest.newBuilder().setEnabled(true).build(), h) }
                                sec.value = next
                                shownEntrance = "$origin/${next.entrance}"
                            }
                        }
                    },
                    variant = ButtonVariant.Outline, size = ButtonSize.Small, loading = action.busy == "entrance" && !closingEntrance,
                )
            }
        }
    }

    if (unbinding) {
        val domain = settings.value?.domain.orEmpty()
        NagiDialog(
            title = stringResource(R.string.domain_unbind), text = stringResource(R.string.domain_unbind_confirm, domain),
            confirm = stringResource(R.string.domain_unbind), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "unbind",
            onDismiss = { unbinding = false },
            onConfirm = {
                action.run("unbind") {
                    settings.value = session.rpc { c, h -> c.settings.setPanelDomain(SetPanelDomainApiRequest.newBuilder().setDomain("").build(), h) }.settings
                    unbinding = false
                }
            },
        )
    }
    if (closingEntrance) {
        NagiDialog(
            title = stringResource(R.string.entrance_disable), text = stringResource(R.string.entrance_disable_confirm),
            confirm = stringResource(R.string.turn_off), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "entrance",
            onDismiss = { closingEntrance = false },
            onConfirm = {
                action.run("entrance") {
                    sec.value = session.rpc { c, h -> c.settings.setEntrance(SetEntranceRequest.newBuilder().setEnabled(false).build(), h) }
                    closingEntrance = false
                }
            },
        )
    }
    shownEntrance?.let { url ->
        // 开启后马上把地址给出来，提示保存（和网页一样，不能点外面关掉）。
        NagiDialog(
            title = stringResource(R.string.entrance_enabled_title), text = stringResource(R.string.entrance_enabled_text),
            confirm = stringResource(R.string.copy), cancel = stringResource(R.string.done),
            onDismiss = { shownEntrance = null },
            onConfirm = { copy(context, url) },
        ) {
            Spacer(Modifier.height(12.dp))
            androidx.compose.material3.Text(url, style = ink.sipc.nagi.ui.NagiTheme.type.mono, color = ink.sipc.nagi.ui.NagiTheme.colors.foreground)
        }
    }
}

/** 对外地址：IP 或域名，不含端口。 */
@Composable
fun AccessAddressScreen(session: PanelSession, onBack: () -> Unit) {
    val settings = rememberLoad { session.rpc { c, h -> c.settings.getSettings(GetSettingsRequest.getDefaultInstance(), h) }.settings }
    SubPage(stringResource(R.string.public_address), onBack) {
        Loaded(settings) { s ->
            EditField(
                initial = s.publicAddress, placeholder = "203.0.113.10", help = stringResource(R.string.public_address_help),
                onDone = onBack, label = stringResource(R.string.public_address),
            ) { v -> session.rpc { c, h -> c.settings.updateSettings(UpdateSettingsRequest.newBuilder().setPublicAddress(v).build(), h) }.let {} }
        }
    }
}

/** 绑定域名：域名须预先解析到面板这台机器。 */
@Composable
fun AccessDomainScreen(session: PanelSession, onBack: () -> Unit) {
    val settings = rememberLoad { session.rpc { c, h -> c.settings.getSettings(GetSettingsRequest.getDefaultInstance(), h) }.settings }
    SubPage(stringResource(R.string.domain), onBack) {
        Loaded(settings) { s ->
            EditField(
                initial = s.domain, placeholder = "panel.example.com", help = stringResource(R.string.domain_help),
                saveLabel = stringResource(R.string.domain_bind), onDone = onBack, label = stringResource(R.string.domain),
            ) { v -> session.rpc { c, h -> c.settings.setPanelDomain(SetPanelDomainApiRequest.newBuilder().setDomain(v).build(), h) }.let {} }
        }
    }
}
