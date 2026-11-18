package io.github.sipc.nagi.ui

import androidx.compose.runtime.Immutable
import androidx.compose.ui.graphics.Color

// 颜色变量，取值同 docs/ui.md 的「颜色」。常态黑白灰，颜色只表示状态；品牌色只用在 logo 和对外流量。
@Immutable
data class NagiColors(
    val background: Color,
    val card: Color,
    val muted: Color,
    val border: Color,
    val foreground: Color,
    val mutedForeground: Color,
    // 放在 muted 底上的次要文字，保证 4.5:1 的对比度
    val mutedForegroundStrong: Color,
    val primary: Color,
    val primaryForeground: Color,
    val destructive: Color,
    val success: Color,
    val warning: Color,
    // 警告色的文字
    val warningText: Color,
    val info: Color,
    val brand: Color,
)

val LightColors = NagiColors(
    background = Color(0xFFFFFFFF),
    card = Color(0xFFFFFFFF),
    muted = Color(0xFFF5F5F5),
    border = Color(0xFFE5E5E5),
    foreground = Color(0xFF0A0A0A),
    mutedForeground = Color(0xFF737373),
    mutedForegroundStrong = Color(0xFF666666),
    primary = Color(0xFF171717),
    primaryForeground = Color(0xFFFAFAFA),
    destructive = Color(0xFFE7000B),
    success = Color(0xFF16A34A),
    warning = Color(0xFFF59E0B),
    warningText = Color(0xFFB45309),
    info = Color(0xFF2563EB),
    brand = Color(0xFF3F9CC6),
)

val DarkColors = NagiColors(
    background = Color(0xFF0A0A0A),
    card = Color(0xFF171717),
    muted = Color(0xFF262626),
    border = Color.White.copy(alpha = 0.10f),
    foreground = Color(0xFFEDEDED),
    mutedForeground = Color(0xFFA1A1A1),
    mutedForegroundStrong = Color(0xFFA1A1A1),
    primary = Color(0xFFE5E5E5),
    primaryForeground = Color(0xFF171717),
    destructive = Color(0xFFFF6467),
    success = Color(0xFF4ADE80),
    warning = Color(0xFFFBBF24),
    warningText = Color(0xFFFBBF24),
    info = Color(0xFF60A5FA),
    brand = Color(0xFF6DB9DC),
)
