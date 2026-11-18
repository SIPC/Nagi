package io.github.sipc.nagi.ui

import androidx.compose.runtime.Immutable
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.LineHeightStyle
import androidx.compose.ui.unit.sp

// MiSans：400 用 Normal，500 用 Regular，600 用 Medium（比字面上的字重细一档，和网页一致，见 docs/android.md 视觉）。
// 字体是子集（fonts/subset.py），缺的字由系统字体补上。
val MiSans = FontFamily(
    Font(R.font.misans_normal, FontWeight.Normal),
    Font(R.font.misans_regular, FontWeight.Medium),
    Font(R.font.misans_medium, FontWeight.SemiBold),
)

// 字号，取值同 docs/android.md 视觉。颜色不写进样式，由使用处或 LocalContentColor 决定。
@Immutable
data class NagiType(
    val body: TextStyle,
    val bodyMedium: TextStyle,
    val secondary: TextStyle,
    val mono: TextStyle,
    val section: TextStyle,
    val topBar: TextStyle,
    val keyNumber: TextStyle,
    val value: TextStyle,
    val button: TextStyle,
    val small: TextStyle,
)

// 行高约 1.45 倍，文字在行内上下居中；数字一律等宽对齐。
private val centered = LineHeightStyle(LineHeightStyle.Alignment.Center, LineHeightStyle.Trim.None)

private fun style(size: Int, line: Int, weight: FontWeight = FontWeight.Normal, family: FontFamily = MiSans) = TextStyle(
    fontFamily = family,
    fontWeight = weight,
    fontSize = size.sp,
    lineHeight = line.sp,
    fontFeatureSettings = "tnum",
    lineHeightStyle = centered,
)

val DefaultType = NagiType(
    body = style(14, 20),
    bodyMedium = style(14, 20, FontWeight.Medium),
    secondary = style(13, 18),
    mono = style(13, 18, family = FontFamily.Monospace),
    section = style(15, 22, FontWeight.SemiBold),
    topBar = style(16, 24, FontWeight.SemiBold),
    keyNumber = style(22, 30, FontWeight.Medium),
    value = style(18, 26, FontWeight.Medium),
    button = style(14, 20, FontWeight.Medium),
    small = style(12, 16),
)
