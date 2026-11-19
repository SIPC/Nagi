package ink.sipc.nagi.settings

import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.height
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.CreateInviteRequest
import ink.sipc.nagi.proto.v1.CreateStandbyCodeRequest
import ink.sipc.nagi.proto.v1.GetStandbysRequest
import ink.sipc.nagi.proto.v1.Invite
import ink.sipc.nagi.proto.v1.ListInvitesRequest
import ink.sipc.nagi.proto.v1.PromoteStandbyRequest
import ink.sipc.nagi.proto.v1.RemoveStandbyRequest
import ink.sipc.nagi.proto.v1.RevokeInviteRequest
import ink.sipc.nagi.proto.v1.StandbyPanel
import ink.sipc.nagi.ui.ActionRow
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiIconButton
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SettingsGroup
import ink.sipc.nagi.ui.StatusDot

// 备用面板、接收节点（管理员，和网页 pages/standby.tsx、pages/invite.tsx 一样）。

/** 给出一条要在别的机器上执行的命令：等宽字，可以复制。 */
@Composable
private fun CommandDialog(title: String, text: String, command: String, notice: String?, onClose: () -> Unit) {
    val context = LocalContext.current
    NagiDialog(
        title = title, text = text, confirm = stringResource(R.string.copy), cancel = stringResource(R.string.done),
        onDismiss = onClose, onConfirm = { copy(context, command) },
    ) {
        Spacer(Modifier.height(12.dp))
        Text(command, style = NagiTheme.type.mono, color = NagiTheme.colors.foreground)
        if (notice != null) {
            Spacer(Modifier.height(8.dp))
            Text(notice, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
        }
    }
}

/**
 * 备用面板：本面板是主面板时列出它的备用面板（连接状态、最近同步，可以移除），右上角加一个（生成配对码，给出命令）；
 * 本面板是备用面板时写主面板和同步状态，可以提升为主面板。
 * 添加和提升面板只认密码或验证码做的确认（v0.35.0 起），手机上做不了：这样的面板上不显示这两个入口，写明在网页上操作。
 */
@Composable
fun StandbyScreen(session: PanelSession, onBack: () -> Unit) {
    val st = rememberLoad { session.rpc { c, h -> c.settings.getStandbys(GetStandbysRequest.getDefaultInstance(), h) } }
    val action = rememberAction()
    var code by remember { mutableStateOf<String?>(null) }
    var removing by remember { mutableStateOf<StandbyPanel?>(null) }
    var promoting by remember { mutableStateOf(false) }
    var promoted by remember { mutableStateOf(false) }
    // 打开、关上确认框时清掉上一次的错误，免得别的操作的错误写进这个确认框。
    LaunchedEffect(removing, promoting) { action.error = null }
    val webOnly = rememberWebOnly(session)
    SubPage(stringResource(R.string.standby), onBack, actions = {
        if (st.value?.isStandby == false && webOnly == false) {
            NagiIconButton(NagiIcons.Plus, stringResource(R.string.standby_add), {
                action.run("code", passwordOnly = true) { code = session.rpc { c, h -> c.settings.createStandbyCode(CreateStandbyCodeRequest.getDefaultInstance(), h) }.code }
            })
        }
    }) {
        ActionError(action)
        if (promoted) Note(stringResource(R.string.standby_promoted))
        Loaded(st) { s ->
            val c = NagiTheme.colors
            if (s.isStandby) {
                Note(stringResource(R.string.standby_status_help))
                ActionRow(stringResource(R.string.standby_primary), description = s.primaryAddress)
                ActionRow(
                    stringResource(R.string.standby_connection),
                    description = listOfNotNull(
                        stringResource(if (s.connected) R.string.standby_connected else R.string.standby_disconnected),
                        if (s.hasLastSync() && s.lastSync.seconds > 0) stringResource(R.string.standby_last_sync, ago(s.lastSync.seconds)) else stringResource(R.string.standby_never_synced),
                        s.lastError.ifEmpty { null },
                    ).joinToString(" · "),
                ) { StatusDot(if (s.connected) c.success else c.warning) }
                if (webOnly == true) {
                    Note(stringResource(R.string.standby_promote_on_web))
                } else if (webOnly == false) {
                    SettingsGroup(null) {
                        NagiButton(stringResource(R.string.standby_promote), { promoting = true }, variant = ButtonVariant.Destructive)
                    }
                }
            } else {
                Note(stringResource(R.string.standby_list_help))
                if (webOnly == true) Note(stringResource(R.string.standby_add_on_web))
                if (s.standbysCount == 0) Note(stringResource(R.string.standby_empty))
                s.standbysList.forEach { p ->
                    ActionRow(
                        p.name.ifEmpty { p.address },
                        description = listOfNotNull(
                            p.address.takeIf { p.name.isNotEmpty() },
                            stringResource(if (p.connected) R.string.standby_connected else R.string.standby_disconnected),
                            if (p.hasLastSync() && p.lastSync.seconds > 0) stringResource(R.string.standby_last_sync, ago(p.lastSync.seconds)) else null,
                        ).joinToString(" · "),
                    ) {
                        StatusDot(if (p.connected) c.success else c.warning)
                        NagiButton(stringResource(R.string.remove), { removing = p }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    }
                }
            }
        }
    }
    // 配对码用环境变量传，不出现在进程参数里（见 docs/standby.md）；v0.35.0 之前的 nagi 不读这个环境变量，提示里写明。
    code?.let {
        CommandDialog(
            stringResource(R.string.standby_add), stringResource(R.string.standby_dialog_desc), "NAGI_STANDBY=$it nagi server",
            stringResource(R.string.standby_dialog_notice),
        ) { code = null; st.reload() }
    }
    removing?.let { p ->
        NagiDialog(
            title = stringResource(R.string.standby_remove), text = stringResource(R.string.standby_remove_confirm, p.name.ifEmpty { p.address }),
            confirm = stringResource(R.string.remove), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "remove",
            onDismiss = { removing = null },
            onConfirm = {
                action.run("remove") {
                    session.rpc { c, h -> c.settings.removeStandby(RemoveStandbyRequest.newBuilder().setId(p.id).build(), h) }
                    removing = null
                    st.reload()
                }
            },
        ) { ActionError(action) }
    }
    if (promoting) {
        NagiDialog(
            title = stringResource(R.string.standby_promote), text = stringResource(R.string.standby_promote_confirm),
            confirm = stringResource(R.string.standby_promote), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "promote",
            onDismiss = { promoting = false },
            onConfirm = {
                action.run("promote", passwordOnly = true) {
                    session.rpc { c, h -> c.settings.promoteStandby(PromoteStandbyRequest.getDefaultInstance(), h) }
                    promoting = false
                    promoted = true
                }
            },
        ) { ActionError(action) }
    }
}

/** 接收节点：有效的接入码（用了几次、什么时候到期，可以撤销），右上角生成一个。 */
@Composable
fun InviteScreen(session: PanelSession, onBack: () -> Unit) {
    val invites = rememberLoad { session.rpc { c, h -> c.settings.listInvites(ListInvitesRequest.getDefaultInstance(), h) }.invitesList }
    val action = rememberAction()
    var created by remember { mutableStateOf<String?>(null) }
    var revoking by remember { mutableStateOf<Invite?>(null) }
    SubPage(stringResource(R.string.invite), onBack, actions = {
        NagiIconButton(NagiIcons.Plus, stringResource(R.string.invite_generate), {
            action.run("create") {
                created = session.rpc { c, h -> c.settings.createInvite(CreateInviteRequest.getDefaultInstance(), h) }.invite
                invites.reload()
            }
        })
    }) {
        ActionError(action)
        Note(stringResource(R.string.invite_desc))
        Loaded(invites) { list ->
            if (list.isEmpty()) Note(stringResource(R.string.invite_empty))
            list.forEach { inv ->
                ActionRow(
                    inv.id,
                    description = listOf(
                        stringResource(R.string.invite_used, inv.uses),
                        stringResource(R.string.invite_expires, dateTime(inv.expiresAt.seconds)),
                    ).joinToString(" · "),
                ) {
                    NagiButton(stringResource(R.string.invite_revoke), { revoking = inv }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                }
            }
        }
    }
    created?.let {
        CommandDialog(stringResource(R.string.invite_generate), stringResource(R.string.invite_paste_hint), it, stringResource(R.string.invite_help)) { created = null }
    }
    revoking?.let { inv ->
        NagiDialog(
            title = stringResource(R.string.invite_revoke_title), text = stringResource(R.string.invite_revoke_confirm, inv.id),
            confirm = stringResource(R.string.invite_revoke), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "revoke",
            onDismiss = { revoking = null },
            onConfirm = {
                action.run("revoke") {
                    session.rpc { c, h -> c.settings.revokeInvite(RevokeInviteRequest.newBuilder().setId(inv.id).build(), h) }
                    revoking = null
                    invites.reload()
                }
            },
        )
    }
}
