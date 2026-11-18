package io.github.sipc.nagi.pairing

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import io.github.sipc.nagi.ui.ButtonVariant
import io.github.sipc.nagi.ui.Dimens
import io.github.sipc.nagi.ui.NagiButton
import io.github.sipc.nagi.ui.NagiLogo
import io.github.sipc.nagi.ui.NagiTheme

/** 欢迎页（s5 第 1 张）：第一次打开、或者手机已退出时。 */
@Composable
fun WelcomeScreen(onScan: () -> Unit, notice: String? = null) {
    Column(
        Modifier.fillMaxSize().statusBarsPadding().navigationBarsPadding().padding(horizontal = Dimens.pageHorizontal),
        verticalArrangement = Arrangement.Bottom,
    ) {
        Spacer(Modifier.weight(1f))
        NagiLogo(Modifier.size(44.dp))
        Spacer(Modifier.height(16.dp))
        Text(
            stringResource(R.string.welcome_title),
            style = TextStyle(fontFamily = NagiTheme.type.section.fontFamily, fontSize = 24.sp, fontWeight = FontWeight.SemiBold, lineHeight = 32.sp),
            color = NagiTheme.colors.foreground,
        )
        Spacer(Modifier.height(8.dp))
        Text(stringResource(R.string.welcome_text), style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground)
        if (notice != null) {
            Spacer(Modifier.height(16.dp))
            Text(notice, style = NagiTheme.type.body, color = NagiTheme.colors.foreground)
        }
        Spacer(Modifier.weight(1f))
        NagiButton(stringResource(R.string.welcome_scan), onScan, Modifier.fillMaxWidth(), variant = ButtonVariant.Primary)
        Spacer(Modifier.height(24.dp))
    }
}
