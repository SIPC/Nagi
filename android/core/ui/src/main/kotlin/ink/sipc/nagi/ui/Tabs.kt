package ink.sipc.nagi.ui

import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Rect
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.layout.onPlaced
import androidx.compose.ui.layout.positionInParent
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.toSize

// UnderlineTabs：选择项（时间范围、日志级别、筛选）。只有文字，选中的是主文字色加 500 字重，下面一条 2 的下划线；
// 其余是次要文字色，没有底色。看得见的高度 32，可点区域由 Compose 按最小触摸尺寸 48 扩展。
// 切换时下划线从上一项滑过去（朝移动方向的边先到，另一条边跟上，见 Motion.kt），文字颜色过渡 150ms。
// 选择项只换数据范围，下面的图表、列表不滑动（docs/ui.md「不滑动」）。
@Composable
fun UnderlineTabs(
    options: List<String>,
    selected: Int,
    onSelect: (Int) -> Unit,
    modifier: Modifier = Modifier,
    spacing: Dp = 16.dp,
) {
    val c = NagiTheme.colors
    val bounds = remember { mutableStateMapOf<Int, Rect>() }
    val slider = rememberSlidingIndicator(selected)
    val line = c.foreground
    Row(
        modifier = modifier
            .height(32.dp)
            .drawBehind {
                // 下划线离底边 3，高 2，圆角 1
                val r = slider.rect(bounds) ?: return@drawBehind
                val h = 2.dp.toPx()
                drawRoundRect(line, Offset(r.left, size.height - 3.dp.toPx() - h), Size(r.width, h), CornerRadius(1.dp.toPx()))
            }
            .selectableGroup(),
        horizontalArrangement = Arrangement.spacedBy(spacing),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        options.forEachIndexed { i, label ->
            val on = i == selected
            val color by animateColorAsState(if (on) c.foreground else c.mutedForeground, tween(150), label = "tab")
            Box(
                modifier = Modifier
                    .height(32.dp)
                    .onPlaced { bounds[i] = Rect(it.positionInParent(), it.size.toSize()) }
                    .selectable(
                        selected = on,
                        interactionSource = remember { MutableInteractionSource() },
                        indication = null,
                        role = Role.Tab,
                        onClick = { if (!on) onSelect(i) },
                    ),
                contentAlignment = Alignment.Center,
            ) {
                Text(
                    label,
                    style = NagiTheme.type.body.copy(fontWeight = if (on) FontWeight.Medium else FontWeight.Normal),
                    color = color,
                    maxLines = 1,
                )
            }
        }
    }
}

// PageTabs：页内标签（节点页的监控、容器，容器页的概览、日志……）。高 40，标签之间 24，下面一条横贯整页的细线
// （伸进页面左右的边距），选中的一项是主文字色加 500 字重，下面一条 2 的下划线压在细线上，从上一项滑过去。
// 放在页面内容里时，[modifier] 不要带左右内边距：组件自己按 Dimens.pageHorizontal 排文字。
@Composable
fun PageTabs(
    options: List<String>,
    selected: Int,
    onSelect: (Int) -> Unit,
    modifier: Modifier = Modifier,
) {
    val c = NagiTheme.colors
    val bounds = remember { mutableStateMapOf<Int, Rect>() }
    val slider = rememberSlidingIndicator(selected)
    val line = c.foreground
    val border = c.border
    Row(
        modifier = modifier
            .height(40.dp)
            .drawBehind {
                val px = 1.dp.toPx()
                drawRect(border, Offset(0f, size.height - px), Size(size.width, px))
            }
            .padding(horizontal = Dimens.pageHorizontal)
            .drawBehind {
                // 在内边距里面画：和各项的位置（相对于这一层）用同一个坐标。
                val r = slider.rect(bounds) ?: return@drawBehind
                val h = 2.dp.toPx()
                drawRect(line, Offset(r.left, size.height - h), Size(r.width, h))
            }
            .selectableGroup(),
        horizontalArrangement = Arrangement.spacedBy(24.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        options.forEachIndexed { i, label ->
            val on = i == selected
            val color by animateColorAsState(if (on) c.foreground else c.mutedForeground, tween(150), label = "page-tab")
            Box(
                modifier = Modifier
                    .fillMaxHeight()
                    .onPlaced { bounds[i] = Rect(it.positionInParent(), it.size.toSize()) }
                    .selectable(
                        selected = on,
                        interactionSource = remember { MutableInteractionSource() },
                        indication = null,
                        role = Role.Tab,
                        onClick = { if (!on) onSelect(i) },
                    ),
                contentAlignment = Alignment.Center,
            ) {
                Text(label, style = NagiTheme.type.body.copy(fontWeight = if (on) FontWeight.Medium else FontWeight.Normal), color = color, maxLines = 1)
            }
        }
    }
}
