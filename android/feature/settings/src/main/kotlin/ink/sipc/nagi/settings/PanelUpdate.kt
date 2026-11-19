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
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.CheckPanelUpdateRequest
import ink.sipc.nagi.proto.v1.GetPanelReleaseNotesRequest
import ink.sipc.nagi.proto.v1.GetUpdateStatusRequest
import ink.sipc.nagi.proto.v1.UpdatePanelRequest
import ink.sipc.nagi.proto.v1.UpdateState
import ink.sipc.nagi.ui.ActionRow
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.formatBytesText
import ink.sipc.nagi.ui.formatRateText
import kotlinx.coroutines.delay

// 面板的更新（管理员，和网页 pages/update.tsx 的面板部分一样）：检查更新、更新说明、更新面板和进度。
// 更新在面板上进行，离开这一页不影响；面板重启时连接会断开，App 自己重连。

@Composable
internal fun PanelUpdateSection(session: PanelSession, canCheck: Boolean) {
    val check = rememberLoad { session.rpc { c, h -> c.settings.checkPanelUpdate(CheckPanelUpdateRequest.getDefaultInstance(), h) } }
    val status = rememberLoad { session.rpc { c, h -> c.settings.getUpdateStatus(GetUpdateStatusRequest.getDefaultInstance(), h) } }
    val action = rememberAction()
    var confirming by remember { mutableStateOf(false) }
    val running = status.value?.panel?.phase.orEmpty() in setOf("queued", "installing", "restarting")
    // 正在更新时每 2 秒查一次进度。
    LaunchedEffect(running) {
        while (running) {
            delay(2_000)
            status.reload()
        }
    }
    ActionError(action)
    val st = status.value?.panel
    if (st != null && st.phase.isNotEmpty()) {
        UpdateProgress(st)
        if (st.phase == "failed") {
            NagiButton(stringResource(R.string.update_retry), { confirming = true }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
        }
        if (running) return
    }
    Loaded(check, rows = 1) { u ->
        val checked = if (u.hasCheckedAt() && u.checkedAt.seconds > 0) stringResource(R.string.update_checked, ago(u.checkedAt.seconds)) else null
        val source = u.latestSource.takeIf { it.isNotEmpty() }?.let { stringResource(R.string.update_query_source, it) }
        when {
            u.unsupported.isNotEmpty() -> ActionRow(stringResource(R.string.update), description = u.unsupported)
            u.available -> ActionRow(stringResource(R.string.update_available), description = listOfNotNull(u.latest, source).joinToString(" · ")) {
                NagiButton(stringResource(R.string.update_action), { confirming = true }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
            }
            else -> ActionRow(stringResource(R.string.update_up_to_date), description = listOfNotNull(checked, source).joinToString(" · ")) {
                if (canCheck) NagiButton(stringResource(R.string.update_check), {
                    action.run("check") { check.value = session.rpc { c, h -> c.settings.checkPanelUpdate(CheckPanelUpdateRequest.newBuilder().setForce(true).build(), h) } }
                }, variant = ButtonVariant.Outline, size = ButtonSize.Small, loading = action.busy == "check")
            }
        }
        if (u.available) ReleaseNotes(session)
        if (confirming) {
            NagiDialog(
                title = stringResource(R.string.update_panel), text = stringResource(R.string.update_confirm, u.latest),
                confirm = stringResource(R.string.update_action), cancel = stringResource(R.string.cancel), busy = action.busy == "update",
                onDismiss = { confirming = false },
                onConfirm = {
                    action.run("update") {
                        session.rpc { c, h -> c.settings.updatePanel(UpdatePanelRequest.getDefaultInstance(), h) }
                        confirming = false
                        status.reload()
                    }
                },
            )
        }
    }
}

/** 更新的进度：下载中 12 MB / 40 MB · 3.2 MB/s、核对中、试运行中、重启中，失败时写原因。 */
@Composable
private fun UpdateProgress(st: UpdateState) {
    val title = when (st.phase) {
        "restarting" -> stringResource(R.string.update_restarting, st.target)
        "failed" -> stringResource(R.string.update_failed, st.target)
        else -> stringResource(R.string.update_installing, st.target)
    }
    val detail = when {
        st.phase == "failed" -> st.error
        st.phase == "restarting" -> st.source.takeIf { it.isNotEmpty() }?.let { stringResource(R.string.update_from, it) }.orEmpty()
        st.stage == "verify" -> stringResource(R.string.update_verify)
        st.stage == "test" -> stringResource(R.string.update_test)
        st.stage == "download" -> {
            val size = if (st.total > 0) "${formatBytesText(st.downloaded.toDouble())} / ${formatBytesText(st.total.toDouble())}" else formatBytesText(st.downloaded.toDouble())
            val speed = if (st.bytesPerSecond > 0) " · ${formatRateText(st.bytesPerSecond.toDouble())}" else ""
            stringResource(R.string.update_download, size + speed) + (st.source.takeIf { it.isNotEmpty() }?.let { " · $it" } ?: "")
        }
        else -> stringResource(R.string.update_installing_text)
    }
    ActionRow(title, description = detail)
}

/** 比本面板新的各版本的更新说明，新的在前。正文是更新日志的 Markdown，去掉标记按行显示。 */
@Composable
private fun ReleaseNotes(session: PanelSession) {
    val notes = rememberLoad { session.rpc { c, h -> c.settings.getPanelReleaseNotes(GetPanelReleaseNotesRequest.getDefaultInstance(), h) }.releasesList }
    val n = notes.value ?: return
    n.forEach { r ->
        Spacer(Modifier.height(8.dp))
        Text(r.version, style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground)
        r.body.lines().map { it.trim() }.filter { it.isNotEmpty() }.forEach { line ->
            val heading = line.startsWith("#")
            Text(
                if (heading) line.trimStart('#', ' ') else "· " + line.removePrefix("- ").removePrefix("* "),
                style = if (heading) NagiTheme.type.secondary else NagiTheme.type.body,
                color = if (heading) NagiTheme.colors.mutedForeground else NagiTheme.colors.foreground,
                modifier = Modifier.then(if (heading) Modifier else Modifier),
            )
        }
    }
}
