package ink.sipc.nagi.ui

import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import kotlin.math.abs
import kotlin.math.min
import kotlin.math.sign

data class ChartLine(val values: List<Float>, val color: Color)

// LineChart：监控折线图（ui.md 监控图表）。平滑曲线（monotone），线宽 1.5，不画网格、纵轴和填充。
// 所有线共用一个纵向比例，从 0 到最大值。下面是横轴的时间（通常 5 个），第一个靠左，最后一个靠右。
// 少于两个点的线不画。
@Composable
fun LineChart(
    series: List<ChartLine>,
    modifier: Modifier = Modifier,
    height: Dp = 112.dp,
    xLabels: List<String> = emptyList(),
) {
    Column(modifier.fillMaxWidth()) {
        Canvas(Modifier.fillMaxWidth().height(height)) {
            val max = series.maxOfOrNull { it.values.maxOrNull() ?: 0f } ?: 0f
            val stroke = 1.5.dp.toPx()
            // 上下各留半个线宽，线贴边时不被裁掉
            val top = stroke / 2
            val bottom = size.height - stroke / 2
            val span = bottom - top
            for (line in series) {
                val v = line.values
                if (v.size < 2) continue
                val dx = size.width / (v.size - 1)
                val ys = FloatArray(v.size) { i -> if (max > 0f) bottom - (v[i].coerceAtLeast(0f) / max) * span else bottom }
                drawPath(
                    monotonePath(dx, ys),
                    line.color,
                    style = Stroke(width = stroke, cap = StrokeCap.Round, join = StrokeJoin.Round),
                )
            }
        }
        if (xLabels.isNotEmpty()) {
            Row(
                Modifier.fillMaxWidth().padding(top = 4.dp),
                horizontalArrangement = Arrangement.SpaceBetween,
            ) {
                for (label in xLabels) {
                    Text(label, style = NagiTheme.type.small, color = NagiTheme.colors.mutedForeground, maxLines = 1)
                }
            }
        }
    }
}

// 单调三次曲线（同 d3 的 curveMonotoneX，即 Recharts 的 type="monotone"），横向等距。不会冲过数据点的高低。
private fun monotonePath(dx: Float, ys: FloatArray): Path {
    val n = ys.size
    val s = FloatArray(n - 1) { (ys[it + 1] - ys[it]) / dx }
    val m = FloatArray(n)
    for (i in 1 until n - 1) {
        val s0 = s[i - 1]
        val s1 = s[i]
        m[i] = (sign(s0) + sign(s1)) * min(min(abs(s0), abs(s1)), 0.5f * abs((s0 + s1) / 2))
    }
    if (n == 2) {
        m[0] = s[0]
        m[1] = s[0]
    } else {
        m[0] = (3 * s[0] - m[1]) / 2
        m[n - 1] = (3 * s[n - 2] - m[n - 2]) / 2
    }
    val p = Path()
    p.moveTo(0f, ys[0])
    for (i in 0 until n - 1) {
        val x0 = i * dx
        val x1 = x0 + dx
        p.cubicTo(x0 + dx / 3, ys[i] + m[i] * dx / 3, x1 - dx / 3, ys[i + 1] - m[i + 1] * dx / 3, x1, ys[i + 1])
    }
    return p
}

// ChartLegend：图例，10 的方块（圆角 2）加 12 号文字，各项间隔 12。
@Composable
fun ChartLegend(items: List<Pair<String, Color>>, modifier: Modifier = Modifier) {
    Row(modifier, horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
        for ((label, color) in items) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                Box(Modifier.size(10.dp).background(color, RoundedCornerShape(2.dp)))
                Text(label, style = NagiTheme.type.small, color = NagiTheme.colors.mutedForeground, maxLines = 1)
            }
        }
    }
}

// UsageBar：细的横向条，muted 底。用在流量最大的映射（品牌色）和占用。长度变化过渡 220ms。
@Composable
fun UsageBar(fraction: Float, color: Color, modifier: Modifier = Modifier, height: Dp = 3.dp) {
    val f by animateFloatAsState(fraction.coerceIn(0f, 1f), tween(220), label = "usage")
    val shape = RoundedCornerShape(2.dp)
    Box(modifier.fillMaxWidth().height(height).clip(shape).background(NagiTheme.colors.muted)) {
        if (f > 0f) Box(Modifier.fillMaxWidth(f).fillMaxHeight().clip(shape).background(color))
    }
}
