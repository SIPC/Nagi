package io.github.sipc.nagi.ui

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.core.animateDpAsState
import androidx.compose.animation.core.snap
import androidx.compose.animation.core.tween
import androidx.compose.animation.expandHorizontally
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.shrinkHorizontally
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.collectIsPressedAsState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.layout.wrapContentWidth
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.dropShadow
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.shadow.Shadow
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.DpOffset
import androidx.compose.ui.unit.dp

data class PillNavItem(val icon: ImageVector, val label: String, val badge: Boolean = false)

// PillNav：底部悬浮的胶囊导航（docs/android.md 导航）。按内容宽度在底部居中，距手势条 8，高 48，每项高 40。
// 平时只显示图标；选中的一项在图标右边写出名称（间距 4），底色是 muted 的胶囊，名称平滑展开和收起。
// 系统开了「移除动画」时名称直接出现。badge 是图标右上角的红点（有严重告警时）。
@Composable
fun PillNav(
    items: List<PillNavItem>,
    selected: Int,
    onSelect: (Int) -> Unit,
    modifier: Modifier = Modifier,
) {
    val c = NagiTheme.colors
    val dark = LocalNagiDark.current
    val reduced = reducedMotion()
    Box(
        modifier = modifier.fillMaxWidth().navigationBarsPadding().padding(bottom = 8.dp),
        contentAlignment = Alignment.BottomCenter,
    ) {
        Row(
            modifier = Modifier
                .wrapContentWidth()
                .height(Dimens.pillHeight)
                .dropShadow(CircleShape, Shadow(radius = 16.dp, color = Color.Black, offset = DpOffset(0.dp, 4.dp), alpha = if (dark) 0.5f else 0.08f))
                .clip(CircleShape)
                .background(c.card)
                .border(1.dp, c.border, CircleShape)
                .padding(3.dp)
                .selectableGroup(),
            horizontalArrangement = Arrangement.spacedBy(4.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            items.forEachIndexed { i, item ->
                PillItem(item, on = i == selected, reduced = reduced) { onSelect(i) }
            }
        }
    }
}

@Composable
private fun PillItem(item: PillNavItem, on: Boolean, reduced: Boolean, onClick: () -> Unit) {
    val c = NagiTheme.colors
    val interaction = remember { MutableInteractionSource() }
    val pressed by interaction.collectIsPressedAsState()
    val color = if (on) c.foreground else c.mutedForeground
    val spec = if (reduced) snap<Dp>() else tween(200)
    val start by animateDpAsState(if (on) 12.dp else 0.dp, spec, label = "start")
    val end by animateDpAsState(if (on) 14.dp else 0.dp, spec, label = "end")
    Row(
        modifier = Modifier
            .height(Dimens.pillItemHeight)
            .widthIn(min = 44.dp)
            .clip(CircleShape)
            .background(if (on) c.muted else Color.Transparent)
            .pressable(true, Role.Tab, null, interaction, pressed, onClick)
            .clearAndSetSemantics {
                contentDescription = item.label
                role = Role.Tab
                selected = on
            }
            .padding(start = start, end = end),
        horizontalArrangement = Arrangement.Center,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box {
            Icon(item.icon, contentDescription = null, tint = color, modifier = Modifier.size(20.dp))
            if (item.badge) {
                // 7 的红点，外面一圈 2 的 card 色描边，和图标分开
                Box(
                    Modifier
                        .align(Alignment.TopEnd)
                        .offset(x = 4.dp, y = (-3).dp)
                        .size(11.dp)
                        .clip(CircleShape)
                        .background(c.card)
                        .padding(2.dp)
                        .clip(CircleShape)
                        .background(c.destructive),
                )
            }
        }
        AnimatedVisibility(
            visible = on,
            enter = if (reduced) fadeIn(snap()) else expandHorizontally(tween(200)) + fadeIn(tween(200)),
            exit = if (reduced) fadeOut(snap()) else shrinkHorizontally(tween(200)) + fadeOut(tween(120)),
        ) {
            Text(
                item.label,
                style = NagiTheme.type.bodyMedium,
                color = c.foreground,
                maxLines = 1,
                modifier = Modifier.padding(start = 4.dp),
            )
        }
    }
}

// BottomFade：胶囊后面的底部渐隐，高 96，从透明渐变到背景色，内容滚到这里变淡。放在胶囊下面一层。
@Composable
fun BottomFade(modifier: Modifier = Modifier) {
    val bg = NagiTheme.colors.background
    Box(
        modifier
            .fillMaxWidth()
            .height(Dimens.bottomFade)
            .background(Brush.verticalGradient(0f to bg.copy(alpha = 0f), 0.55f to bg, 1f to bg)),
    )
}
