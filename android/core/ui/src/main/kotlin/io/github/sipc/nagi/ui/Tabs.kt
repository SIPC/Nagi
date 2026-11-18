package io.github.sipc.nagi.ui

import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

// UnderlineTabs：选择项（时间范围、日志级别、筛选）。只有文字，选中的是主文字色加 500 字重，下面一条 2 的下划线；
// 其余是次要文字色，没有底色。看得见的高度 32，可点区域由 Compose 按最小触摸尺寸 48 扩展。
@Composable
fun UnderlineTabs(
    options: List<String>,
    selected: Int,
    onSelect: (Int) -> Unit,
    modifier: Modifier = Modifier,
    spacing: Dp = 16.dp,
) {
    val c = NagiTheme.colors
    Row(
        modifier = modifier.height(32.dp).selectableGroup(),
        horizontalArrangement = Arrangement.spacedBy(spacing),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        options.forEachIndexed { i, label ->
            val on = i == selected
            val line = c.foreground
            Box(
                modifier = Modifier
                    .height(32.dp)
                    .selectable(
                        selected = on,
                        interactionSource = remember { MutableInteractionSource() },
                        indication = null,
                        role = Role.Tab,
                        onClick = { if (!on) onSelect(i) },
                    )
                    .drawBehind {
                        // 下划线离底边 3，圆角 1
                        if (on) {
                            val h = 2.dp.toPx()
                            drawRoundRect(line, Offset(0f, size.height - 3.dp.toPx() - h), Size(size.width, h), CornerRadius(1.dp.toPx()))
                        }
                    },
                contentAlignment = Alignment.Center,
            ) {
                Text(
                    label,
                    style = NagiTheme.type.body.copy(fontWeight = if (on) FontWeight.Medium else FontWeight.Normal),
                    color = if (on) c.foreground else c.mutedForeground,
                    maxLines = 1,
                )
            }
        }
    }
}
