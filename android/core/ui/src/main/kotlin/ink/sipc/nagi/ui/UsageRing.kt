package ink.sipc.nagi.ui

import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.material3.Icon
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp

/**
 * UsageRing：一项占用（首页的节点状态、节点列表）。26 的圆环，中间是这一项的图标（CPU、内存、磁盘），不写数字。
 * 到 80% 变警告色、90% 变红；不知道总量时（[fraction] 为 null）只有灰色的底环。弧的长度变化有过渡。读屏读「CPU 16%」。
 * 放在 muted 底上时（选中的一行）[track] 传 border，底环才看得见。
 */
@Composable
fun UsageRing(label: String, icon: ImageVector, fraction: Double?, modifier: Modifier = Modifier, track: Color = NagiTheme.colors.muted) {
    val c = NagiTheme.colors
    val f = fraction?.takeIf { it.isFinite() }?.coerceIn(0.0, 1.0)
    val color = when {
        f == null -> c.mutedForeground
        f >= 0.9 -> c.destructive
        f >= 0.8 -> c.warning
        else -> c.brand
    }
    val sweep by animateFloatAsState(((f ?: 0.0) * 360).toFloat(), barSpec(), label = "ring")
    Box(
        modifier.size(26.dp).semantics(mergeDescendants = true) { contentDescription = "$label ${f?.let { "%.0f%%".format(it * 100) } ?: "—"}" },
        contentAlignment = Alignment.Center,
    ) {
        Canvas(Modifier.size(26.dp)) {
            val stroke = 2.5.dp.toPx()
            val inset = stroke / 2
            val arc = Size(size.width - stroke, size.height - stroke)
            drawArc(track, 0f, 360f, false, Offset(inset, inset), arc, style = Stroke(stroke))
            if (sweep > 0f) drawArc(color, -90f, sweep, false, Offset(inset, inset), arc, style = Stroke(stroke, cap = StrokeCap.Round))
        }
        Icon(icon, contentDescription = null, tint = c.mutedForeground, modifier = Modifier.size(12.dp))
    }
}
