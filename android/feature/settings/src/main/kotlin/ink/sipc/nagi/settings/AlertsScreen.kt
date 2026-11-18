package ink.sipc.nagi.settings

import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.AlertChannel
import ink.sipc.nagi.proto.v1.AlertConfig
import ink.sipc.nagi.proto.v1.GetAlertConfigRequest
import ink.sipc.nagi.proto.v1.GetAuditSettingsRequest
import ink.sipc.nagi.proto.v1.TestAlertChannelRequest
import ink.sipc.nagi.proto.v1.UpdateAlertConfigRequest
import ink.sipc.nagi.ui.ActionRow
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiIconButton
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SettingsGroup
import ink.sipc.nagi.ui.SwitchLabels
import ink.sipc.nagi.ui.SwitchRow
import ink.sipc.nagi.ui.UnderlineTabs
import java.util.UUID

// 告警（管理员，和网页 pages/alerts.tsx 一样）：通知渠道（webhook、Telegram）和登录安全告警的三个开关。
// 告警规则在各节点和项目的设置里编辑，这里不列。保存渠道时只发渠道（和网页一样），面板保留规则。

private suspend fun saveChannels(session: PanelSession, channels: List<AlertChannel>) {
    session.rpc { c, h ->
        c.settings.updateAlertConfig(UpdateAlertConfigRequest.newBuilder().setConfig(AlertConfig.newBuilder().addAllChannels(channels)).build(), h)
    }
}

@Composable
fun AlertsScreen(session: PanelSession, onBack: () -> Unit, open: (String) -> Unit) {
    val cfg = rememberLoad { session.rpc { c, h -> c.settings.getAlertConfig(GetAlertConfigRequest.getDefaultInstance(), h) } }
    val audit = rememberLoad { session.rpc { c, h -> c.settings.getAuditSettings(GetAuditSettingsRequest.getDefaultInstance(), h) } }
    val action = rememberAction()
    var tested by remember { mutableStateOf<String?>(null) }
    val testSent = stringResource(R.string.channel_test_sent)
    val switch = SwitchLabels(stringResource(R.string.on_status), stringResource(R.string.off_status), stringResource(R.string.turn_on), stringResource(R.string.turn_off))
    SubPage(stringResource(R.string.alerts), onBack, actions = {
        if (cfg.value != null) NagiIconButton(NagiIcons.Plus, stringResource(R.string.channel_add), { open(SettingsRoutes.channel("new")) })
    }) {
        ActionError(action)
        tested?.let { Note(it) }
        Loaded(cfg) { r ->
            if (r.lost) {
                Text(stringResource(R.string.alerts_lost), style = NagiTheme.type.body, color = NagiTheme.colors.warningText, modifier = Modifier.fillMaxWidth())
            }
            Note(stringResource(R.string.alerts_channels_help))
            val channels = r.config.channelsList
            if (channels.isEmpty()) Note(stringResource(R.string.channels_empty))
            channels.forEach { ch ->
                ActionRow(ch.name, description = if (ch.type == "telegram") "Telegram" else "Webhook") {
                    NagiButton(stringResource(R.string.channel_test), {
                        action.run("test-${ch.id}") {
                            session.rpc { c, h -> c.settings.testAlertChannel(TestAlertChannelRequest.newBuilder().setChannel(ch).build(), h) }
                            tested = testSent.format(ch.name)
                        }
                    }, variant = ButtonVariant.Ghost, size = ButtonSize.Small, loading = action.busy == "test-${ch.id}")
                    NagiButton(stringResource(R.string.edit), { open(SettingsRoutes.channel(ch.id)) }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                }
            }
            Note(stringResource(R.string.alerts_rules_where))
        }
        SettingsGroup(stringResource(R.string.security_alerts)) {
            Loaded(audit) { a ->
                SwitchRow(
                    stringResource(R.string.security_alert_totp), a.alertTotpFailures,
                    { action.run("sa") { audit.value = session.rpc { c, h -> c.settings.setAuditSettings(a.toBuilder().setAlertTotpFailures(!a.alertTotpFailures).build(), h) } } },
                    switch, description = stringResource(R.string.security_alert_totp_desc), busy = action.busy == "sa",
                )
                SwitchRow(
                    stringResource(R.string.security_alert_ratelimit), a.alertLoginRatelimit,
                    { action.run("sa") { audit.value = session.rpc { c, h -> c.settings.setAuditSettings(a.toBuilder().setAlertLoginRatelimit(!a.alertLoginRatelimit).build(), h) } } },
                    switch, description = stringResource(R.string.security_alert_ratelimit_desc), busy = action.busy == "sa",
                )
                SwitchRow(
                    stringResource(R.string.security_alert_network), a.alertAdminNetwork,
                    { action.run("sa") { audit.value = session.rpc { c, h -> c.settings.setAuditSettings(a.toBuilder().setAlertAdminNetwork(!a.alertAdminNetwork).build(), h) } } },
                    switch, description = stringResource(R.string.security_alert_network_desc), busy = action.busy == "sa",
                )
                Note(stringResource(R.string.security_alerts_always))
            }
        }
    }
}

/** 加或者改一个渠道：名称、类型、地址（webhook）或机器人令牌和聊天 ID（Telegram）、允许内网地址。[id] 为 "new" 时是加。 */
@Composable
fun ChannelScreen(session: PanelSession, id: String, onBack: () -> Unit) {
    val cfg = rememberLoad { session.rpc { c, h -> c.settings.getAlertConfig(GetAlertConfigRequest.getDefaultInstance(), h) } }
    val action = rememberAction()
    val adding = id == "new"
    var removing by remember { mutableStateOf(false) }
    SubPage(stringResource(if (adding) R.string.channel_add else R.string.channel_edit), onBack) {
        Loaded(cfg) { r ->
            val initial = r.config.channelsList.firstOrNull { it.id == id } ?: AlertChannel.newBuilder().setType("webhook").build()
            var name by remember(initial) { mutableStateOf(initial.name) }
            var type by remember(initial) { mutableStateOf(initial.type.ifEmpty { "webhook" }) }
            var url by remember(initial) { mutableStateOf(initial.url) }
            var token by remember(initial) { mutableStateOf(initial.telegramToken) }
            var chat by remember(initial) { mutableStateOf(initial.telegramChatId) }
            var allowPrivate by remember(initial) { mutableStateOf(initial.allowPrivate) }
            fun channel() = initial.toBuilder().setId(initial.id.ifEmpty { UUID.randomUUID().toString().take(8) }).setName(name.trim()).setType(type)
                .setUrl(if (type == "webhook") url.trim() else "").setTelegramToken(if (type == "telegram") token.trim() else "")
                .setTelegramChatId(if (type == "telegram") chat.trim() else "").setAllowPrivate(type == "webhook" && allowPrivate).build()
            Spacer(Modifier.height(Dimens.topBarToContent + 8.dp))
            Field(stringResource(R.string.channel_name)) { NagiTextField(name, { name = it }, label = stringResource(R.string.channel_name), maxLength = 64) }
            Field(stringResource(R.string.channel_type)) {
                UnderlineTabs(listOf("Webhook", "Telegram"), if (type == "telegram") 1 else 0, { type = if (it == 1) "telegram" else "webhook" })
            }
            if (type == "webhook") {
                Field(stringResource(R.string.channel_url)) { NagiTextField(url, { url = it }, label = stringResource(R.string.channel_url), placeholder = "https://") }
                SwitchRow(
                    stringResource(R.string.channel_allow_private), allowPrivate, { allowPrivate = !allowPrivate },
                    SwitchLabels(stringResource(R.string.on_status), stringResource(R.string.off_status), stringResource(R.string.turn_on), stringResource(R.string.turn_off)),
                    description = stringResource(R.string.channel_allow_private_help),
                )
            } else {
                Field(stringResource(R.string.channel_bot_token)) { NagiTextField(token, { token = it }, label = stringResource(R.string.channel_bot_token), password = true) }
                Field(stringResource(R.string.channel_chat_id)) { NagiTextField(chat, { chat = it }, label = stringResource(R.string.channel_chat_id)) }
            }
            ActionError(action)
            Spacer(Modifier.height(16.dp))
            NagiButton(stringResource(R.string.channel_test), {
                action.run("test") { session.rpc { c, h -> c.settings.testAlertChannel(TestAlertChannelRequest.newBuilder().setChannel(channel()).build(), h) }.let {} }
            }, Modifier.fillMaxWidth(), variant = ButtonVariant.Outline, loading = action.busy == "test")
            Spacer(Modifier.height(12.dp))
            NagiButton(stringResource(R.string.save), {
                action.run("save") {
                    val ch = channel()
                    val list = r.config.channelsList.toMutableList()
                    val i = list.indexOfFirst { it.id == ch.id }
                    if (i >= 0) list[i] = ch else list += ch
                    saveChannels(session, list)
                    onBack()
                }
            }, Modifier.fillMaxWidth(), loading = action.busy == "save", enabled = name.isNotBlank() && (if (type == "webhook") url.isNotBlank() else token.isNotBlank() && chat.isNotBlank()))
            if (!adding) {
                Spacer(Modifier.height(12.dp))
                NagiButton(stringResource(R.string.channel_delete), { removing = true }, Modifier.fillMaxWidth(), variant = ButtonVariant.Destructive)
            }
            if (removing) {
                NagiDialog(
                    title = stringResource(R.string.channel_delete), text = stringResource(R.string.channel_delete_confirm, initial.name),
                    confirm = stringResource(R.string.delete), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "delete",
                    onDismiss = { removing = false },
                    onConfirm = {
                        action.run("delete") {
                            saveChannels(session, r.config.channelsList.filter { it.id != initial.id })
                            removing = false
                            onBack()
                        }
                    },
                )
            }
        }
    }
}

/** 表单的一项：上面灰色的名称，下面是控件，项之间 12。 */
@Composable
internal fun Field(label: String, content: @Composable () -> Unit) {
    Text(label, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.height(22.dp))
    content()
    Spacer(Modifier.height(12.dp))
}
