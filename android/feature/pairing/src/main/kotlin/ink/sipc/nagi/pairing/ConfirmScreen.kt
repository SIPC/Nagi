package ink.sipc.nagi.pairing

import ink.sipc.nagi.ui.Motion
import androidx.compose.animation.core.tween
import androidx.compose.animation.Crossfade
import android.graphics.BitmapFactory
import androidx.compose.foundation.Image
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.runtime.remember
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
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SkeletonRows

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
            // 骨架换成内容时淡入（160ms），不突然冒出来。
            val phase = when {
                s.loading -> 0
                info == null && s.error != null -> 1
                info != null -> 2
                else -> 3
            }
            Crossfade(targetState = phase, animationSpec = tween(Motion.FADE_IN, easing = Motion.Enter), label = "confirm") { ph ->
                Column {
                    when (ph) {
                        0 -> SkeletonRows(3)
                        else -> ConfirmBody(s, info, vm)
                    }
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

/** 账号：头像（成员在面板上传的，没有时是首字母加渐变色）、显示名称和角色。 */
@Composable
private fun Member(name: String, role: String, avatar: ByteArray) {
    // 面板只接受 PNG、JPEG、WebP（不超过 256 KB），系统都能解。解不开时按没有头像处理。
    val image = remember(avatar) {
        if (avatar.isEmpty()) null else runCatching { BitmapFactory.decodeByteArray(avatar, 0, avatar.size)?.asImageBitmap() }.getOrNull()
    }
    Row(verticalAlignment = Alignment.CenterVertically) {
        if (image != null) {
            Image(image, contentDescription = null, contentScale = ContentScale.Crop, modifier = Modifier.size(40.dp).clip(CircleShape))
        } else {
            Box(
                Modifier.size(40.dp).background(Brush.linearGradient(listOf(Color(0xFF8B5CF6), Color(0xFFEC4899))), CircleShape),
                contentAlignment = Alignment.Center,
            ) {
                Text(name.take(1).uppercase(), style = NagiTheme.type.bodyMedium, color = Color.White)
            }
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

/** 确认页的内容：查询失败的提示，或成员和面板的信息。 */
@Composable
private fun ConfirmBody(s: ConfirmState, info: ink.sipc.nagi.proto.v1.GetPairingResponse?, vm: PairingViewModel) {
    when {
        info == null && s.error != null -> {
            Text(
                if (s.unreachable) stringResource(R.string.confirm_unreachable, s.error.orEmpty()) else s.error.orEmpty(),
                style = NagiTheme.type.body, color = NagiTheme.colors.destructive,
            )
            Spacer(Modifier.height(12.dp))
            NagiButton(stringResource(R.string.retry), vm::retry, variant = ButtonVariant.Outline)
        }
        info != null -> {
            Member(info.displayName.ifEmpty { info.username }, roleLabel(info.role), info.avatar.toByteArray())
            Spacer(Modifier.height(20.dp))
            val uri = s.uri!!
            Field(stringResource(R.string.confirm_address), uri.address, mono = true)
            if (uri.domain.isNotEmpty()) Field(stringResource(R.string.confirm_domain), uri.domain, mono = true)
            Field(stringResource(R.string.confirm_fingerprint), shortFingerprint(s.fingerprint ?: uri.fingerprint), mono = true)
            Field(stringResource(R.string.confirm_phone), vm.deviceLabel, mono = false)
        }
    }
}
