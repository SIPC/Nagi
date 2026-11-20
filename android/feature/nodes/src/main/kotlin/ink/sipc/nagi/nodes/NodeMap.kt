package ink.sipc.nagi.nodes

import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.State
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ColorFilter
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.graphics.drawscope.Fill
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.translate
import androidx.compose.ui.graphics.painter.Painter
import androidx.compose.ui.graphics.Matrix
import androidx.compose.ui.graphics.vector.PathParser
import androidx.compose.ui.graphics.vector.rememberVectorPainter
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.TextLayoutResult
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.drawText
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.Constraints
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.MAP_PANEL
import ink.sipc.nagi.data.MapLayout
import ink.sipc.nagi.data.MapPoint
import ink.sipc.nagi.data.mapLabelOnSide
import ink.sipc.nagi.data.mapLabelWidth
import ink.sipc.nagi.data.usageInfo
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.ui.Motion
import ink.sipc.nagi.ui.NagiColors
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.barSpec
import ink.sipc.nagi.ui.mouseHover
import kotlin.math.abs
import kotlin.math.hypot
import kotlin.math.roundToInt
import kotlin.math.sign

// 拓扑图（docs/android.md「宽屏」，同网页的 node-map.tsx）：面板在最上面，和节点围成一圈，用 Canvas 画。
// 网络、高可用两个图层共用这里的节点、面板、线和标签的画法，位置相同（core/data 的 mapLayout）。
// 按布局的实际大小画，不缩放（缩放会让文字小于 12）；比可用宽度宽时横向滚动。

/** 画拓扑图的最小可用宽度：窗口 840，减去宽屏左侧 88 的一栏。 */
val MapMinWidth: Dp = 752.dp

/** 一条线：从 [from] 经 [control] 到 [to] 的二次曲线。[dash] 是虚线的线段和间隔（dp），[head] 是箭头的三个顶点。 */
internal class MapLine(
    val key: String,
    val from: MapPoint,
    val control: MapPoint,
    val to: MapPoint,
    val color: Color,
    val alpha: Float,
    val dash: FloatArray? = null,
    val head: List<MapPoint>? = null,
)

/** 线上的标签（延迟、容器数），带圆角底框，线从下面穿过也不会露在字缝里。 */
internal class MapTag(val key: String, val at: MapPoint, val text: String, val color: Color, val alpha: Float)

/**
 * 一个节点。[sub] 是名称下面的说明，[warn] 接在说明后面、用警告色写（无副本的容器数）；[failed] 是高可用图里模拟故障的节点；
 * [hover] 时内圈加深。
 */
internal class MapGlyph(val node: Node, val sub: String, val alpha: Float, val warn: String = "", val failed: Boolean = false, val hover: Boolean = false)

internal val DashRelay = floatArrayOf(6f, 5f)
internal val DashNone = floatArrayOf(2f, 5f)

// 面板的标志：两道波浪，同 NagiLogo（网页的 Waves），路径在 24 × 14 里。
private val waveLow = PathParser().parsePathString("M2 10c3-3 6-3 10 0s7 3 10 0").toPath()
private val waveHigh = PathParser().parsePathString("M2 4c3-3 6-3 10 0s7 3 10 0").toPath()

/**
 * 拓扑图。[onHover] 是鼠标（触控板、触控笔）悬浮在哪个节点或面板上（[MAP_PANEL]），移开时为 null；
 * [onTap] 是点了哪个节点或面板，点空白处为 null。透明度变化有过渡（150ms 左右，同网页），外圈的占用变化同占用条。
 */
@Composable
internal fun MapCanvas(
    layout: MapLayout,
    avail: Dp,
    description: String,
    lines: List<MapLine>,
    tags: List<MapTag>,
    glyphs: List<MapGlyph>,
    panelAlpha: Float,
    onHover: (String?) -> Unit,
    onTap: (String?) -> Unit,
) {
    val c = NagiTheme.colors
    val type = NagiTheme.type
    val density = LocalDensity.current
    val measurer = rememberTextMeasurer(cacheSize = 64)
    val box = layout.box
    val fade = tween<Float>(Motion.FADE_IN, easing = Motion.Enter)
    val ring = barSpec()

    // 透明度、外圈都按 key 各自过渡，在绘制时读，变化时只重画。
    val lineAlpha = HashMap<String, State<Float>>()
    for (l in lines) key("l" + l.key) { lineAlpha[l.key] = animateFloatAsState(l.alpha, fade, label = "line") }
    val tagAlpha = HashMap<String, State<Float>>()
    for (t in tags) key("t" + t.key) { tagAlpha[t.key] = animateFloatAsState(t.alpha, fade, label = "tag") }
    val nodeAlpha = HashMap<String, State<Float>>()
    val nodeRing = HashMap<String, State<Float>>()
    for (g in glyphs) key("n" + g.node.id) {
        nodeAlpha[g.node.id] = animateFloatAsState(g.alpha, fade, label = "node")
        val u = g.node.usageInfo()
        nodeRing[g.node.id] = animateFloatAsState(u?.max?.toFloat()?.coerceIn(0f, 1f) ?: 0f, ring, label = "ring")
    }
    val panelFade by animateFloatAsState(panelAlpha, fade, label = "panel")

    // 文字在重组时量好：放不下时截短，末尾加省略号。
    val nameStyle = type.bodyMedium
    val subStyle = type.small
    val tagStyle = type.small.copy(fontFeatureSettings = "tnum")
    val texts = glyphs.map { g ->
        val at = layout.pos.getValue(g.node.id)
        val max = with(density) { mapLabelWidth(at, box, layout.spacing).dp.roundToPx() }
        fun measure(s: AnnotatedString, style: TextStyle) =
            measurer.measure(s, style, overflow = TextOverflow.Ellipsis, softWrap = false, maxLines = 1, constraints = Constraints(maxWidth = max))
        // 说明量两份：带警告色的一段用来写字，不带颜色的用来垫背景（描边会沿用文字里的颜色）。
        val sub = if (g.sub.isEmpty()) null else {
            val full = if (g.warn.isEmpty()) g.sub else g.sub + " · " + g.warn
            val colored = buildAnnotatedString {
                append(g.sub)
                if (g.warn.isNotEmpty()) withStyle(SpanStyle(color = c.warningText)) { append(" · " + g.warn) }
            }
            measure(colored, subStyle) to measure(AnnotatedString(full), subStyle)
        }
        GlyphText(measure(AnnotatedString(g.node.name), nameStyle), sub?.first, sub?.second)
    }
    val tagTexts = tags.map { measurer.measure(it.text, tagStyle, maxLines = 1) }
    val panelText = measurer.measure(stringResource(R.string.map_panel), nameStyle, maxLines = 1)
    val server = rememberVectorPainter(NagiIcons.Server)

    // 点和悬浮落在哪个节点上（坐标是图上的 dp）：节点按外圈外面一点算，面板按方框。
    val hit by rememberUpdatedState { px: Offset ->
        val x = px.x / density.density + box.x
        val y = px.y / density.density + box.y
        val panel = layout.pos.getValue(MAP_PANEL)
        when {
            abs(x - panel.x) <= 28 && abs(y - panel.y) <= 28 -> MAP_PANEL
            else -> glyphs.firstOrNull { g -> layout.pos[g.node.id]?.let { hypot(x - it.x, y - it.y) <= 30 } == true }?.node?.id
        }
    }
    val hover by rememberUpdatedState(onHover)
    val tap by rememberUpdatedState(onTap)

    val overflow = box.w > avail.value
    Box(
        Modifier.fillMaxWidth().then(if (overflow) Modifier.horizontalScroll(rememberScrollState()) else Modifier),
        contentAlignment = Alignment.TopCenter,
    ) {
        Canvas(
            Modifier
                .size(box.w.dp, box.h.dp)
                .semantics { contentDescription = description }
                .mouseHover(onMove = { hover(hit(it)) }, onExit = { hover(null) })
                .pointerInput(Unit) { detectTapGestures { tap(hit(it)) } },
        ) {
            val dp = density.density
            fun p(m: MapPoint) = Offset((m.x - box.x) * dp, (m.y - box.y) * dp)
            val stroke = 1.5f * dp

            for (l in lines) {
                val a = lineAlpha[l.key]?.value ?: l.alpha
                val path = Path().apply {
                    val from = p(l.from)
                    val ctl = p(l.control)
                    val to = p(l.to)
                    moveTo(from.x, from.y)
                    quadraticTo(ctl.x, ctl.y, to.x, to.y)
                }
                val effect = l.dash?.let { d -> PathEffect.dashPathEffect(floatArrayOf(d[0] * dp, d[1] * dp)) }
                drawPath(path, l.color, alpha = a, style = Stroke(stroke, cap = StrokeCap.Round, pathEffect = effect))
                l.head?.let { h ->
                    val tri = Path().apply {
                        val p0 = p(h[0]); val p1 = p(h[1]); val p2 = p(h[2])
                        moveTo(p0.x, p0.y); lineTo(p1.x, p1.y); lineTo(p2.x, p2.y); close()
                    }
                    drawPath(tri, l.color, alpha = a)
                }
            }

            tags.forEachIndexed { i, t ->
                val a = tagAlpha[t.key]?.value ?: t.alpha
                val text = tagTexts[i]
                val w = text.size.width + 12 * dp
                val h = 18 * dp
                val at = p(t.at)
                val tl = Offset(at.x - w / 2, at.y - h / 2)
                drawRoundRect(c.background, tl, Size(w, h), CornerRadius(9 * dp), alpha = a)
                drawRoundRect(c.border, tl, Size(w, h), CornerRadius(9 * dp), alpha = a, style = Stroke(1 * dp))
                drawText(text, t.color, Offset(at.x - text.size.width / 2f, at.y - text.size.height / 2f), alpha = a)
            }

            drawPanel(c, p(layout.pos.getValue(MAP_PANEL)), panelFade, panelText, dp)

            glyphs.forEachIndexed { i, g ->
                val at = layout.pos.getValue(g.node.id)
                drawGlyph(c, g, at, p(at), nodeAlpha[g.node.id]?.value ?: g.alpha, nodeRing[g.node.id]?.value ?: 0f, texts[i], server, dp)
            }
        }
    }
}

private fun DrawScope.halo(text: TextLayoutResult, color: Color, topLeft: Offset, alpha: Float, dp: Float) {
    // 文字下面垫一圈背景色，压在线上也看得清。描边样式会留在这份排版结果上，之后画正文要显式传 Fill。
    drawText(text, color, topLeft, alpha = alpha, drawStyle = Stroke(4 * dp, join = StrokeJoin.Round))
}

private fun DrawScope.drawPanel(c: NagiColors, at: Offset, alpha: Float, label: TextLayoutResult, dp: Float) {
    val tl = Offset(at.x - 24 * dp, at.y - 24 * dp)
    drawRoundRect(c.card, tl, Size(48 * dp, 48 * dp), CornerRadius(8 * dp), alpha = alpha)
    drawRoundRect(c.border, tl, Size(48 * dp, 48 * dp), CornerRadius(8 * dp), alpha = alpha, style = Stroke(1 * dp))
    // 标志宽 26，按 24 × 14 等比缩放。
    val k = 26f / 24f * dp
    translate(at.x - 13 * dp, at.y - 7 * k) {
        val scale = Matrix().apply { scale(k, k) }
        val low = Path().apply { addPath(waveLow); transform(scale) }
        val high = Path().apply { addPath(waveHigh); transform(scale) }
        drawPath(low, c.brand, alpha = alpha, style = Stroke(2.4f * k, cap = StrokeCap.Round))
        drawPath(high, c.foreground, alpha = alpha * 0.55f, style = Stroke(2.4f * k, cap = StrokeCap.Round))
    }
    val top = Offset(at.x - label.size.width / 2f, at.y - 34 * dp - label.firstBaseline)
    halo(label, c.background, top, alpha, dp)
    drawText(label, c.foreground, top, alpha = alpha, drawStyle = Fill)
}

/**
 * 一个节点：外圈是 CPU、内存、磁盘里占用最高的一项（到 75% 变黄、90% 变红，同占用条），中间是服务器图标，右下角是状态点。
 * 离线的整体淡一些；文字写在圆的外侧（两侧的写在旁边，其余写在上方或下方）。
 */
private fun DrawScope.drawGlyph(
    c: NagiColors,
    g: MapGlyph,
    at: MapPoint,
    center: Offset,
    alpha: Float,
    ratio: Float,
    text: GlyphText,
    server: Painter,
    dp: Float,
) {
    val n = g.node
    val live = n.online && !n.pending
    val a = alpha * if (!n.online && !n.pending && !g.failed) 0.6f else 1f
    drawCircle(c.background, 25 * dp, center, alpha = a)
    drawCircle(c.foreground.copy(alpha = 0.1f), 25 * dp, center, alpha = a, style = Stroke(3 * dp))
    if (live && ratio > 0f) {
        val color = when {
            ratio >= 0.9f -> c.destructive
            ratio >= 0.75f -> c.warning
            else -> c.primary
        }
        drawArc(
            color, -90f, maxOf(ratio, 0.02f) * 360f, useCenter = false,
            topLeft = Offset(center.x - 25 * dp, center.y - 25 * dp), size = Size(50 * dp, 50 * dp),
            alpha = a, style = Stroke(3 * dp, cap = StrokeCap.Round),
        )
    }
    drawCircle(c.card, 19 * dp, center, alpha = a)
    val ringColor = when {
        g.failed -> c.destructive
        n.pending -> c.warning
        g.hover -> c.foreground.copy(alpha = 0.4f)
        else -> c.border
    }
    drawCircle(
        ringColor, 19 * dp, center, alpha = a,
        style = Stroke(if (g.failed) 2 * dp else 1 * dp, pathEffect = if (n.pending) PathEffect.dashPathEffect(floatArrayOf(3 * dp, 3 * dp)) else null),
    )
    val icon = when {
        g.failed -> c.destructive
        live -> c.foreground
        else -> c.mutedForeground
    }
    translate(center.x - 8 * dp, center.y - 8 * dp) {
        with(server) { draw(Size(16 * dp, 16 * dp), alpha = a, colorFilter = ColorFilter.tint(icon)) }
    }
    val dot = Offset(center.x + 14 * dp, center.y + 14 * dp)
    val dotColor = when {
        g.failed -> c.destructive
        n.pending -> c.warning
        n.online -> c.success
        else -> c.mutedForeground.copy(alpha = 0.6f)
    }
    drawCircle(dotColor, 4.5f * dp, dot, alpha = a)
    drawCircle(c.background, 4.5f * dp, dot, alpha = a, style = Stroke(2 * dp))

    // 文字的基线和网页一样：两侧的名称在圆心上下，上方的写在圆上面，下方的写在圆下面。
    val name = text.name
    val sub = text.sub
    val side = mapLabelOnSide(at)
    val above = at.y < 0
    val textX = if (side) sign(at.x) * 34 else 0f
    val nameY = if (side) (if (sub != null) -3f else 4f) else if (above) (if (sub != null) -50f else -34f) else 46f
    val subY = if (side) 13f else if (above) -34f else 62f
    fun left(t: TextLayoutResult): Float {
        val x = center.x + textX * dp
        return when {
            !side -> x - t.size.width / 2f
            at.x > 0 -> x
            else -> x - t.size.width
        }
    }
    val nameTop = Offset(left(name), center.y + nameY * dp - name.firstBaseline)
    halo(name, c.background, nameTop, a, dp)
    drawText(name, c.foreground, nameTop, alpha = a, drawStyle = Fill)
    if (sub != null) {
        val subTop = Offset(left(sub), center.y + subY * dp - sub.firstBaseline)
        halo(text.subHalo ?: sub, c.background, subTop, a, dp)
        drawText(sub, c.mutedForeground, subTop, alpha = a, drawStyle = Fill)
    }
}

/** 节点的名称和说明，量好的。 */
private class GlyphText(val name: TextLayoutResult, val sub: TextLayoutResult?, val subHalo: TextLayoutResult?)

/** 图例里的一项：一小段线（虚线、带箭头）和说明。 */
internal class LegendEntry(val text: String, val color: Color, val dash: FloatArray? = null, val arrow: Boolean = false, val textColor: Color? = null)

/** 图例：在图下面居中，12 号灰字。 */
@Composable
internal fun MapLegend(items: List<LegendEntry>) {
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(16.dp, Alignment.CenterHorizontally), verticalAlignment = Alignment.CenterVertically) {
        for (e in items) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Canvas(Modifier.size(24.dp, 8.dp)) {
                    val dp = density
                    val end = if (e.arrow) 17 * dp else 23 * dp
                    drawLine(
                        e.color, Offset(1 * dp, 4 * dp), Offset(end, 4 * dp), strokeWidth = 1.5f * dp, cap = StrokeCap.Round,
                        pathEffect = e.dash?.let { d -> PathEffect.dashPathEffect(floatArrayOf(d[0] * dp, d[1] * dp)) },
                    )
                    if (e.arrow) {
                        drawPath(Path().apply { moveTo(23 * dp, 4 * dp); lineTo(15 * dp, 0f); lineTo(15 * dp, 8 * dp); close() }, e.color)
                    }
                }
                Text(e.text, style = NagiTheme.type.small, color = e.textColor ?: NagiTheme.colors.mutedForeground, maxLines = 1)
            }
        }
    }
}

// ---- 两个图层共用的文字 ----

/** 离线、待批准的节点在名称下面写状态，在线时为空，由图层自己写。 */
@Composable
internal fun mapStatusLine(n: Node): String = when {
    n.pending -> stringResource(R.string.pending)
    !n.online -> if (n.lastSeen.seconds > 0) stringResource(R.string.map_last_seen, agoText(n.lastSeen.seconds)) else stringResource(R.string.map_never_seen)
    else -> ""
}

/** 百分比取整，同网页拓扑图的写法。[f] 是 0–1。 */
internal fun pct(f: Double): String = "${(f * 100).roundToInt()}%"

/** 节点网络的路径。 */
@Composable
internal fun pathLabel(path: String): String = when (path) {
    "direct" -> stringResource(R.string.map_path_direct)
    "link" -> stringResource(R.string.map_path_link)
    "node-relay" -> stringResource(R.string.map_path_node_relay)
    "udp-relay" -> stringResource(R.string.map_path_udp_relay)
    "tls-relay" -> stringResource(R.string.map_path_tls_relay)
    "none" -> stringResource(R.string.map_path_none)
    else -> path
}
