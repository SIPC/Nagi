package ink.sipc.nagi.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.layout.layout
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

/** 可点的行按下时的底色比内容左右各宽出多少（伸进页面左右 20 的边距里）。 */
val RowInset = 12.dp

/**
 * rowClickable：整行可点（列表行、设置行、数值块）。按下的底色比内容左右各宽出 [inset]，圆角 8，不贴着内容的边缘截断，
 * 和网页列表行悬停的底色一样（docs/ui.md「列表行」）。内容的位置和这一行占的宽度不变。
 */
fun Modifier.rowClickable(onClick: () -> Unit, role: Role? = null, enabled: Boolean = true, inset: Dp = RowInset): Modifier = this
    // 量的时候左右各多给 inset，摆的时候往左挪 inset，报给外面的宽度还是原来的。
    .layout { measurable, constraints ->
        val extra = inset.roundToPx() * 2
        val wide = constraints.copy(
            minWidth = constraints.minWidth + extra,
            maxWidth = if (constraints.hasBoundedWidth) constraints.maxWidth + extra else constraints.maxWidth,
        )
        val p = measurable.measure(wide)
        layout(p.width - extra, p.height) { p.place(-extra / 2, 0) }
    }
    .clip(RoundedCornerShape(8.dp))
    .clickable(enabled = enabled, role = role, onClick = onClick)
    .padding(horizontal = inset)
