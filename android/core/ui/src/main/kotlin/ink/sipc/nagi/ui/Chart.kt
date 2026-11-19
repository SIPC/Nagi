package ink.sipc.nagi.ui

import androidx.compose.foundation.layout.IntrinsicSize
import kotlin.math.roundToInt
import androidx.compose.ui.unit.Constraints
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.layout.layout
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.draw.shadow
import androidx.compose.runtime.setValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.mutableStateOf
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.gestures.waitForUpOrCancellation
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.gestures.awaitEachGesture
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

/** 一条线。[label] 写在提示里。 */
data class ChartLine(val values: List<Float>, val color: Color, val label: String = "")

// LineChart：监控折线图（ui.md 监控图表）。平滑曲线（monotone），线宽 1.5，不画网格、纵轴和填充。
// 所有线共用一个纵向比例，从 0 到最大值。下面是横轴的时间（通常 5 个），第一个靠左，最后一个靠右。
// 少于两个点的线不画。
// 给了 [tipTime] 时可以看某个时刻的数值（同网页）：点一下看点的那一刻，再点一下收起；长按后左右拖动逐个看；
// 接鼠标时悬浮就看，移出收起。对准的时刻画一条竖的虚线，提示写时间和每条线的数值（[tipFormat]），
// 在虚线右边，过了一半放到左边。
@Composable
fun LineChart(
    series: List<ChartLine>,
    modifier: Modifier = Modifier,
    height: Dp = 112.dp,
    xLabels: List<String> = emptyList(),
    tipTime: ((Int) -> String)? = null,
    tipFormat: (Float) -> String = { it.toString() },
) {
    val count = series.maxOfOrNull { it.values.size } ?: 0
    var hover by remember { mutableStateOf<Int?>(null) }
    if (hover != null && hover!! >= count) hover = null
    val density = LocalDensity.current
    val colors = NagiTheme.colors
    Column(modifier.fillMaxWidth()) {
        BoxWithConstraints(Modifier.fillMaxWidth().height(height)) {
        val widthPx = constraints.maxWidth.toFloat()
        val nearest = { x: Float -> if (count < 2) null else ((x / widthPx) * (count - 1)).roundToInt().coerceIn(0, count - 1) }
        val input = if (tipTime == null || count < 2) Modifier else Modifier
            .mouseHover(onMove = { hover = nearest(it.x) }, onExit = { hover = null })
            .pointerInput(count, widthPx) {
                awaitEachGesture {
                    val down = awaitFirstDown(requireUnconsumed = false)
                    // 1：松手（点一下）；2：被页面滚动拿走；null：按住超过长按时间。
                    val r = withTimeoutOrNull(viewConfiguration.longPressTimeoutMillis) { if (waitForUpOrCancellation() != null) 1 else 2 }
                    when (r) {
                        1 -> { val i = nearest(down.position.x); hover = if (hover == i) null else i }
                        null -> {
                            hover = nearest(down.position.x)
                            while (true) {
                                val c = awaitPointerEvent().changes.firstOrNull { it.id == down.id } ?: break
                                if (!c.pressed) break
                                c.consume()
                                hover = nearest(c.position.x)
                            }
                        }
                    }
                }
            }
        Canvas(Modifier.fillMaxWidth().height(height).then(input)) {
            // 不是有限的数（节点乱报，v0.35.0#16）不参与最大值，画在底部。
            val max = series.maxOfOrNull { s -> s.values.filter { it.isFinite() }.maxOrNull() ?: 0f } ?: 0f
            val stroke = 1.5.dp.toPx()
            // 上下各留半个线宽，线贴边时不被裁掉
            val top = stroke / 2
            val bottom = size.height - stroke / 2
            val span = bottom - top
            for (line in series) {
                val v = line.values
                if (v.size < 2) continue
                val dx = size.width / (v.size - 1)
                val ys = FloatArray(v.size) { i -> if (max > 0f && v[i].isFinite()) bottom - (v[i].coerceAtLeast(0f) / max) * span else bottom }
                drawPath(
                    monotonePath(dx, ys),
                    line.color,
                    style = Stroke(width = stroke, cap = StrokeCap.Round, join = StrokeJoin.Round),
                )
            }
            hover?.let { i ->
                val x = size.width * i / (count - 1)
                drawLine(
                    colors.mutedForeground, Offset(x, 0f), Offset(x, size.height), strokeWidth = 1.dp.toPx(),
                    pathEffect = PathEffect.dashPathEffect(floatArrayOf(3.dp.toPx(), 3.dp.toPx())),
                )
            }
        }
        val i = hover
        if (i != null && tipTime != null) {
            val x = widthPx * i / (count - 1)
            val toLeft = x > widthPx / 2
            val gap = with(density) { 8.dp.toPx() }
            Column(
                Modifier
                    .layout { m, c ->
                        val p = m.measure(c.copy(minWidth = 0, maxWidth = Constraints.Infinity))
                        layout(c.maxWidth, p.height) { p.place((if (toLeft) x - gap - p.width else x + gap).roundToInt(), 0) }
                    }
                    .shadow(8.dp, RoundedCornerShape(8.dp))
                    .background(colors.card, RoundedCornerShape(8.dp))
                    .padding(horizontal = 10.dp, vertical = 6.dp)
                    .width(IntrinsicSize.Max),
                verticalArrangement = Arrangement.spacedBy(6.dp),
            ) {
                Text(tipTime(i), style = NagiTheme.type.small, color = colors.mutedForeground, maxLines = 1)
                for (line in series.reversed()) {
                    // 数值靠右对齐，和名称至少隔 12。
                    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                        Box(Modifier.size(10.dp).background(line.color, RoundedCornerShape(2.dp)))
                        Text(line.label, style = NagiTheme.type.small, color = colors.mutedForeground, maxLines = 1, modifier = Modifier.padding(start = 8.dp))
                        Spacer(Modifier.width(12.dp))
                        Spacer(Modifier.weight(1f))
                        Text(line.values.getOrNull(i)?.let(tipFormat) ?: "—", style = NagiTheme.type.small, color = colors.foreground, maxLines = 1)
                    }
                }
            }
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
    val f by animateFloatAsState(if (fraction.isFinite()) fraction.coerceIn(0f, 1f) else 0f, barSpec(), label = "usage")
    val shape = RoundedCornerShape(2.dp)
    Box(modifier.fillMaxWidth().height(height).clip(shape).background(NagiTheme.colors.muted)) {
        if (f > 0f) Box(Modifier.fillMaxWidth(f).fillMaxHeight().clip(shape).background(color))
    }
}
