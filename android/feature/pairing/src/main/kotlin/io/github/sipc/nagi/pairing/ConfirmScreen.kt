package io.github.sipc.nagi.pairing

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.sipc.nagi.ui.ButtonVariant
import io.github.sipc.nagi.ui.Dimens
import io.github.sipc.nagi.ui.NagiBackTopBar
import io.github.sipc.nagi.ui.NagiButton
import io.github.sipc.nagi.ui.NagiTheme
import io.github.sipc.nagi.ui.SkeletonRows

/** 确认添加（s5 第 2 张）：已按二维码里的指纹校验了面板证书，显示面板、成员、地址、指纹和这台手机。 */
@Composable
fun ConfirmScreen(vm: PairingViewModel, onBack: () -> Unit, onDone: () -> Unit) {
    val s by vm.state.collectAsStateWithLifecycle()
    val activity = LocalContext.current as FragmentActivity
    val texts = pairingTexts()
    LaunchedEffect(s.done) { if (s.done) onDone() }
    val info = s.info
    Column(Modifier.fillMaxSize().navigationBarsPadding()) {
        NagiBackTopBar(title = "", onBack = onBack)
        Column(
            Modifier.weight(1f).verticalScroll(rememberScrollState()).padding(horizontal = Dimens.pageHorizontal),
        ) {
            Spacer(Modifier.height(Dimens.topBarToContent))
            Text(
                if (info != null) stringResource(R.string.confirm_title, info.panelName) else stringResource(R.string.confirm_title_loading),
                style = TextStyle(fontFamily = NagiTheme.type.section.fontFamily, fontSize = 22.sp, fontWeight = FontWeight.SemiBold, lineHeight = 30.sp),
                color = NagiTheme.colors.foreground,
            )
            Spacer(Modifier.height(4.dp))
            Text(stringResource(R.string.confirm_text), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            Spacer(Modifier.height(24.dp))
            when {
                s.loading -> SkeletonRows(3)
                info == null && s.error != null -> {
                    Text(
                        if (s.unreachable) stringResource(R.string.confirm_unreachable, s.error.orEmpty()) else s.error.orEmpty(),
                        style = NagiTheme.type.body, color = NagiTheme.colors.destructive,
                    )
                    Spacer(Modifier.height(12.dp))
                    NagiButton(stringResource(R.string.retry), vm::retry, variant = ButtonVariant.Outline)
                }
                info != null -> {
                    Member(info.displayName.ifEmpty { info.username }, roleLabel(info.role))
                    Spacer(Modifier.height(20.dp))
                    val uri = s.uri!!
                    Field(stringResource(R.string.confirm_address), uri.address, mono = true)
                    if (uri.domain.isNotEmpty()) Field(stringResource(R.string.confirm_domain), uri.domain, mono = true)
                    Field(stringResource(R.string.confirm_fingerprint), shortFingerprint(uri.fingerprint), mono = true)
                    Field(stringResource(R.string.confirm_phone), vm.deviceLabel, mono = false)
                }
            }
        }
        Column(Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 16.dp)) {
            if (info != null && s.error != null) {
                Text(s.error.orEmpty(), style = NagiTheme.type.body, color = NagiTheme.colors.destructive)
                Spacer(Modifier.height(12.dp))
            }
            Text(stringResource(R.string.confirm_hint), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            Spacer(Modifier.height(12.dp))
            NagiButton(
                stringResource(R.string.confirm_add), { vm.submit(activity, texts) }, Modifier.fillMaxWidth(),
                enabled = info != null, loading = s.submitting,
            )
        }
    }
}

@Composable
private fun Member(name: String, role: String) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        Box(
            Modifier.size(32.dp).background(Brush.linearGradient(listOf(Color(0xFF8B5CF6), Color(0xFFEC4899))), CircleShape),
            contentAlignment = Alignment.Center,
        ) {
            Text(name.take(1).uppercase(), style = NagiTheme.type.bodyMedium, color = Color.White)
        }
        Spacer(Modifier.width(12.dp))
        Column {
            Text(name, style = NagiTheme.type.body, color = NagiTheme.colors.foreground)
            Text(role, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
        }
    }
}

@Composable
private fun Field(label: String, value: String, mono: Boolean) {
    Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), horizontalArrangement = Arrangement.spacedBy(16.dp)) {
        Text(label, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.width(48.dp))
        Text(value, style = if (mono) NagiTheme.type.mono else NagiTheme.type.body, color = NagiTheme.colors.foreground)
    }
}

/** 指纹太长，只显示开头和结尾（完整的在网页上能对照）：sha256//3f9c6a1e…82d4。 */
internal fun shortFingerprint(fp: String): String {
    val body = fp.removePrefix("sha256//")
    return if (body.length <= 16) fp else "sha256//${body.take(8)}…${body.takeLast(6)}"
}

@Composable
internal fun roleLabel(role: String): String = when (role) {
    "admin" -> stringResource(R.string.role_admin)
    "operator" -> stringResource(R.string.role_operator)
    "viewer" -> stringResource(R.string.role_viewer)
    else -> role
}

@Composable
internal fun pairingTexts(): PairingTexts {
    val title = stringResource(R.string.prompt_add_title)
    val subtitle = stringResource(R.string.prompt_add_subtitle)
    val cancel = stringResource(R.string.cancel)
    val noLock = stringResource(R.string.error_no_lock)
    val keyFailed = stringResource(R.string.error_key_failed)
    return PairingTexts({ title.format(it) }, subtitle, cancel, noLock, { keyFailed.format(it) })
}
