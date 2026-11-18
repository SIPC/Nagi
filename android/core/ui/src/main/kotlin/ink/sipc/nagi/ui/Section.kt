package ink.sipc.nagi.ui

import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

// SectionHeader：区块标题行，高 36，标题 15 / 600，右边放这个区块的控件（时间范围、「全部」、数量）。
@Composable
fun SectionHeader(
    title: String,
    modifier: Modifier = Modifier,
    trailing: @Composable RowScope.() -> Unit = {},
) {
    Row(
        modifier = modifier.fillMaxWidth().height(Dimens.sectionTitleHeight),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            title,
            style = NagiTheme.type.section,
            color = NagiTheme.colors.foreground,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1f).semantics { heading() },
        )
        Row(verticalAlignment = Alignment.CenterVertically, content = trailing)
    }
}

// SectionDivider：大区块之间的细线，上下各 16，宽度是内容区（放在页面左右 20 的内边距里）。
@Composable
fun SectionDivider(modifier: Modifier = Modifier) {
    Box(
        modifier
            .fillMaxWidth()
            .padding(vertical = Dimens.sectionGap)
            .height(1.dp)
            .background(NagiTheme.colors.border),
    )
}

// StatusDot：状态点，默认 6。颜色变化过渡 300ms。
@Composable
fun StatusDot(color: Color, modifier: Modifier = Modifier, size: Dp = 6.dp) {
    val animated by animatedDotColor(color)
    Box(modifier.size(size).clip(CircleShape).background(animated))
}

// Skeleton：加载时的占位块，muted 底、圆角 6。尺寸由调用方给。
@Composable
fun Skeleton(modifier: Modifier = Modifier) {
    Box(modifier.background(NagiTheme.colors.muted, RoundedCornerShape(6.dp)))
}

// SkeletonRows：几行占位，每行高 36，间隔 8。
@Composable
fun SkeletonRows(rows: Int = 2, modifier: Modifier = Modifier) {
    Column(modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        repeat(rows) { Skeleton(Modifier.fillMaxWidth().height(36.dp)) }
    }
}
