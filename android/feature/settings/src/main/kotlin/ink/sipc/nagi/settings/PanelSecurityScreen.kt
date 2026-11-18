package ink.sipc.nagi.settings

import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.res.stringResource
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.AuditSettings
import ink.sipc.nagi.proto.v1.GetAdminNetworksRequest
import ink.sipc.nagi.proto.v1.GetAuditSettingsRequest
import ink.sipc.nagi.proto.v1.GetSessionPolicyRequest
import ink.sipc.nagi.proto.v1.SessionPolicy
import ink.sipc.nagi.proto.v1.VerifyAuditRequest
import ink.sipc.nagi.ui.ActionRow
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.SettingsGroup
import ink.sipc.nagi.ui.SwitchLabels
import ink.sipc.nagi.ui.SwitchRow

// 安全策略（管理员，和网页 pages/panel-security.tsx 一样）：终端录制、审计记录保留、日志外送、哈希链、会话、管理员登录地址。
// 关闭录制、缩短保留时间、改外送目标、改管理员登录地址要先验证身份：面板要求时提示在网页上操作（App 还不能做确认）。
// 外送目标的表单（syslog、HTTP、S3 的地址和密钥）在手机上只列出状态，编辑在网页上。

private val idleOptions = listOf(30, 60, 240, 480, 1440)
private val maxOptions = listOf(24, 72, 168)
private val retentionOptions = listOf(30, 90, 180, 365)
private val longRetentionOptions = listOf(90, 180, 365, 730, 1095)

/** 设置里存的值不在选项里时（例如以后改过选项）也列出来。 */
private fun withOption(options: List<Int>, v: Int) = if (v in options) options else (options + v).sorted()

@Composable
private fun minutesLabel(n: Int) = if (n % 60 == 0) stringResource(R.string.n_hours, n / 60) else stringResource(R.string.n_minutes, n)

@Composable
private fun hoursLabel(n: Int) = if (n % 24 == 0) stringResource(R.string.n_days, n / 24) else stringResource(R.string.n_hours, n)

@Composable
fun PanelSecurityScreen(session: PanelSession, onBack: () -> Unit) {
    val audit = rememberLoad { session.rpc { c, h -> c.settings.getAuditSettings(GetAuditSettingsRequest.getDefaultInstance(), h) } }
    val policy = rememberLoad { session.rpc { c, h -> c.settings.getSessionPolicy(GetSessionPolicyRequest.getDefaultInstance(), h) } }
    val nets = rememberLoad { session.rpc { c, h -> c.settings.getAdminNetworks(GetAdminNetworksRequest.getDefaultInstance(), h) } }
    val action = rememberAction()
    // 缩短保留时间先确认：到期的记录会被删除，不能恢复。
    var shorten by remember { mutableStateOf<Pair<Boolean, Int>?>(null) }
    val switch = SwitchLabels(stringResource(R.string.on_status), stringResource(R.string.off_status), stringResource(R.string.turn_on), stringResource(R.string.turn_off))

    fun saveAudit(next: AuditSettings) = action.run("audit") {
        audit.value = session.rpc { c, h -> c.settings.setAuditSettings(next, h) }
    }
    fun savePolicy(next: SessionPolicy) = action.run("policy") {
        policy.value = session.rpc { c, h -> c.settings.setSessionPolicy(next, h) }
    }

    SubPage(stringResource(R.string.panel_security), onBack) {
        ActionError(action)
        Loaded(audit) { a ->
            SettingsGroup(null) {
                SwitchRow(
                    stringResource(R.string.recording), a.recording, { saveAudit(a.toBuilder().setRecording(!a.recording).build()) }, switch,
                    description = stringResource(R.string.recording_desc), busy = action.busy == "audit",
                )
            }
            SettingsGroup(stringResource(R.string.retention)) {
                SelectRow(
                    stringResource(R.string.retention_normal), withOption(retentionOptions, a.retentionDays), a.retentionDays, { stringResource(R.string.n_days, it) },
                    { v -> if (v < a.retentionDays) shorten = false to v else saveAudit(a.toBuilder().setRetentionDays(v).build()) },
                )
                SelectRow(
                    stringResource(R.string.retention_long), withOption(longRetentionOptions, a.longRetentionDays), a.longRetentionDays, { stringResource(R.string.n_days, it) },
                    { v -> if (v < a.longRetentionDays) shorten = true to v else saveAudit(a.toBuilder().setLongRetentionDays(v).build()) },
                    description = stringResource(R.string.retention_long_desc),
                )
            }
            SettingsGroup(stringResource(R.string.sinks)) {
                if (a.sinksCount == 0) Note(stringResource(R.string.sinks_empty))
                a.sinksList.forEach { s ->
                    val status = when {
                        s.failing -> stringResource(R.string.sink_failing) + if (s.lastError.isNotEmpty()) " · ${s.lastError}" else ""
                        s.hasLastSuccess() && s.lastSuccess.seconds > 0 -> stringResource(R.string.sink_sent, ago(s.lastSuccess.seconds))
                        else -> stringResource(R.string.sink_waiting)
                    }
                    ActionRow(s.name, description = "${sinkType(s.type)} · $status")
                }
                Note(stringResource(R.string.sinks_edit_on_web))
            }
            SettingsGroup(stringResource(R.string.chain)) {
                val c = a.chain
                val verified = when {
                    !c.hasVerifiedAt() || c.verifiedAt.seconds == 0L -> stringResource(R.string.chain_never)
                    c.ok -> stringResource(R.string.chain_ok, c.records, ago(c.verifiedAt.seconds))
                    else -> c.reason
                }
                ActionRow(stringResource(R.string.chain_verify), description = verified) {
                    NagiButton(stringResource(R.string.chain_verify_now), {
                        action.run("verify") {
                            val chain = session.rpc { cl, h -> cl.settings.verifyAudit(VerifyAuditRequest.getDefaultInstance(), h) }
                            audit.value = a.toBuilder().setChain(chain).build()
                        }
                    }, variant = ButtonVariant.Outline, size = ButtonSize.Small, loading = action.busy == "verify")
                }
                ActionRow(
                    stringResource(R.string.chain_anchor),
                    description = if (c.hasAnchorAt() && c.anchorAt.seconds > 0) stringResource(R.string.chain_anchor_at, c.anchorId, ago(c.anchorAt.seconds), c.anchorHash.take(12))
                    else stringResource(R.string.chain_no_anchor),
                )
            }
        }
        SettingsGroup(stringResource(R.string.session_policy)) {
            Loaded(policy) { p ->
                SelectRow(
                    stringResource(R.string.idle), withOption(idleOptions, p.idleMinutes), p.idleMinutes, { minutesLabel(it) },
                    { savePolicy(p.toBuilder().setIdleMinutes(it).build()) }, description = stringResource(R.string.idle_desc),
                )
                SelectRow(
                    stringResource(R.string.max_age), withOption(maxOptions, p.maxHours), p.maxHours, { hoursLabel(it) },
                    { savePolicy(p.toBuilder().setMaxHours(it).build()) }, description = stringResource(R.string.max_age_desc),
                )
                SelectRow(
                    stringResource(R.string.bind), listOf("subnet", "ip", "off"), p.bind,
                    { stringResource(when (it) { "subnet" -> R.string.bind_subnet; "ip" -> R.string.bind_ip; else -> R.string.bind_off }) },
                    { savePolicy(p.toBuilder().setBind(it).build()) }, description = stringResource(R.string.bind_desc),
                )
                SwitchRow(
                    stringResource(R.string.bind_all), p.bindAll, { savePolicy(p.toBuilder().setBindAll(!p.bindAll).build()) }, switch,
                    description = if (p.bind == "off") stringResource(R.string.bind_all_unavailable) else stringResource(R.string.bind_all_desc),
                    busy = action.busy == "policy", enabled = p.bind != "off",
                )
            }
            Loaded(nets, rows = 1) { n ->
                ActionRow(
                    stringResource(R.string.admin_networks),
                    description = (if (n.networksCount > 0) n.networksList.joinToString("、") else stringResource(R.string.admin_networks_off)) +
                        " · " + stringResource(R.string.admin_networks_edit_on_web),
                )
            }
        }
    }

    shorten?.let { (long, days) ->
        val kind = stringResource(if (long) R.string.retention_long_kind else R.string.retention_normal_kind)
        NagiDialog(
            title = stringResource(R.string.retention_shorten), text = stringResource(R.string.retention_shorten_confirm, kind, days),
            confirm = stringResource(R.string.retention_shorten), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "audit",
            onDismiss = { shorten = null },
            onConfirm = {
                val a = audit.value ?: return@NagiDialog
                shorten = null
                saveAudit(if (long) a.toBuilder().setLongRetentionDays(days).build() else a.toBuilder().setRetentionDays(days).build())
            },
        )
    }
}

@Composable
private fun sinkType(type: String) = when (type) {
    "http" -> "HTTP"
    "syslog" -> "syslog"
    "s3" -> stringResource(R.string.sink_s3)
    else -> type
}
