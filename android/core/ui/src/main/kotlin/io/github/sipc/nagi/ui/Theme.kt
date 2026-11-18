package io.github.sipc.nagi.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.ColorScheme
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.LocalTextStyle
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Typography
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.ReadOnlyComposable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.text.TextStyle

private val LocalNagiColors = staticCompositionLocalOf { LightColors }
private val LocalNagiType = staticCompositionLocalOf { DefaultType }
internal val LocalNagiDark = staticCompositionLocalOf { false }

// NagiTheme：颜色和字体。外面包一层 MaterialTheme，偶尔用到的 M3 组件也是黑白灰和 MiSans。
@Composable
fun NagiTheme(darkTheme: Boolean = isSystemInDarkTheme(), content: @Composable () -> Unit) {
    val colors = if (darkTheme) DarkColors else LightColors
    val type = DefaultType
    MaterialTheme(colorScheme = colors.toMaterial(darkTheme), typography = materialTypography()) {
        CompositionLocalProvider(
            LocalNagiColors provides colors,
            LocalNagiType provides type,
            LocalNagiDark provides darkTheme,
            LocalContentColor provides colors.foreground,
            LocalTextStyle provides type.body,
            content = content,
        )
    }
}

object NagiTheme {
    val colors: NagiColors
        @Composable @ReadOnlyComposable get() = LocalNagiColors.current
    val type: NagiType
        @Composable @ReadOnlyComposable get() = LocalNagiType.current
}

private fun NagiColors.toMaterial(dark: Boolean): ColorScheme {
    val base = if (dark) darkColorScheme() else lightColorScheme()
    return base.copy(
        primary = primary,
        onPrimary = primaryForeground,
        primaryContainer = muted,
        onPrimaryContainer = foreground,
        inversePrimary = primaryForeground,
        secondary = primary,
        onSecondary = primaryForeground,
        secondaryContainer = muted,
        onSecondaryContainer = foreground,
        tertiary = brand,
        onTertiary = primaryForeground,
        tertiaryContainer = muted,
        onTertiaryContainer = foreground,
        background = background,
        onBackground = foreground,
        surface = background,
        onSurface = foreground,
        surfaceVariant = muted,
        onSurfaceVariant = mutedForeground,
        surfaceTint = background,
        inverseSurface = primary,
        inverseOnSurface = primaryForeground,
        error = destructive,
        onError = primaryForeground,
        errorContainer = muted,
        onErrorContainer = destructive,
        outline = border,
        outlineVariant = border,
        surfaceBright = card,
        surfaceDim = background,
        surfaceContainer = card,
        surfaceContainerHigh = card,
        surfaceContainerHighest = muted,
        surfaceContainerLow = card,
        surfaceContainerLowest = background,
    )
}

// M3 的各级字号都换成 MiSans，其余照旧。
private fun materialTypography(): Typography {
    val t = Typography()
    fun TextStyle.mi() = copy(fontFamily = MiSans, fontFeatureSettings = "tnum")
    return Typography(
        displayLarge = t.displayLarge.mi(), displayMedium = t.displayMedium.mi(), displaySmall = t.displaySmall.mi(),
        headlineLarge = t.headlineLarge.mi(), headlineMedium = t.headlineMedium.mi(), headlineSmall = t.headlineSmall.mi(),
        titleLarge = t.titleLarge.mi(), titleMedium = t.titleMedium.mi(), titleSmall = t.titleSmall.mi(),
        bodyLarge = t.bodyLarge.mi(), bodyMedium = DefaultType.body, bodySmall = DefaultType.secondary,
        labelLarge = DefaultType.button, labelMedium = t.labelMedium.mi(), labelSmall = DefaultType.small,
    )
}
