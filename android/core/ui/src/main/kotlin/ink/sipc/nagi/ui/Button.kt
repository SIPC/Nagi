package ink.sipc.nagi.ui

import android.provider.Settings
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.collectIsPressedAsState
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp

enum class ButtonVariant { Primary, Outline, Ghost, Destructive }

// Default 高 36，Small 高 32（列表行尾的批准、更新）。
enum class ButtonSize { Default, Small }

// 按下时不透明度 80%，立即生效，没有水波纹和缩放（ui.md 动画）。
internal fun Modifier.pressable(
    enabled: Boolean,
    role: Role,
    onClickLabel: String? = null,
    interaction: MutableInteractionSource,
    pressed: Boolean,
    onClick: () -> Unit,
): Modifier = this
    .graphicsLayer { alpha = if (pressed) 0.8f else 1f }
    .clickable(interactionSource = interaction, indication = null, enabled = enabled, onClickLabel = onClickLabel, role = role, onClick = onClick)

// 系统开了「移除动画」（动画时长缩放为 0）时返回 true，动画改成直接到位。
@Composable
internal fun reducedMotion(): Boolean {
    val context = LocalContext.current
    return remember(context) {
        Settings.Global.getFloat(context.contentResolver, Settings.Global.ANIMATOR_DURATION_SCALE, 1f) == 0f
    }
}

// NagiButton：圆角 6，无阴影。看得见的高度 36 / 32；可点区域由 Compose 按最小触摸尺寸 48 扩展，布局高度不变。
// loading 时用小转圈代替文字，宽度不变。
@Composable
fun NagiButton(
    text: String,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    variant: ButtonVariant = ButtonVariant.Primary,
    size: ButtonSize = ButtonSize.Default,
    enabled: Boolean = true,
    loading: Boolean = false,
) {
    val c = NagiTheme.colors
    val dark = LocalNagiDark.current
    val interaction = remember { MutableInteractionSource() }
    val pressed by interaction.collectIsPressedAsState()
    val shape = RoundedCornerShape(6.dp)
    val (bg, fg) = when (variant) {
        ButtonVariant.Primary -> c.primary to c.primaryForeground
        ButtonVariant.Outline, ButtonVariant.Ghost -> Color.Transparent to c.foreground
        // 深色下红色按钮用更深的 #dc2626，白字才够对比度
        ButtonVariant.Destructive -> (if (dark) Color(0xFFDC2626) else c.destructive) to Color.White
    }
    val weight = if (variant == ButtonVariant.Primary || variant == ButtonVariant.Destructive) FontWeight.Medium else FontWeight.Normal
    Box(
        modifier = modifier
            .alpha(if (enabled) 1f else 0.5f)
            .height(if (size == ButtonSize.Small) Dimens.smallButtonHeight else Dimens.buttonHeight)
            .clip(shape)
            .background(bg, shape)
            .then(if (variant == ButtonVariant.Outline) Modifier.border(BorderStroke(1.dp, c.border), shape) else Modifier)
            .pressable(enabled && !loading, Role.Button, null, interaction, pressed, onClick)
            .padding(horizontal = if (size == ButtonSize.Small) 12.dp else 14.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text,
            style = NagiTheme.type.body.copy(fontWeight = weight),
            color = fg,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.alpha(if (loading) 0f else 1f),
        )
        if (loading) CircularProgressIndicator(modifier = Modifier.size(16.dp), color = fg, strokeWidth = 2.dp)
    }
}

// NagiIconButton：可点区域 48 × 48，图形 20。放在顶部栏最右边时，图形右边缘离屏幕边 20（顶部栏右侧内边距 6）。
@Composable
fun NagiIconButton(
    icon: ImageVector,
    contentDescription: String,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    enabled: Boolean = true,
    tint: Color = NagiTheme.colors.foreground,
) {
    val interaction = remember { MutableInteractionSource() }
    val pressed by interaction.collectIsPressedAsState()
    Box(
        modifier = modifier
            .size(Dimens.iconButton)
            .alpha(if (enabled) 1f else 0.5f)
            .pressable(enabled, Role.Button, contentDescription, interaction, pressed, onClick),
        contentAlignment = Alignment.Center,
    ) {
        Icon(icon, contentDescription = contentDescription, tint = tint, modifier = Modifier.size(20.dp))
    }
}
