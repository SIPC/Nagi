package ink.sipc.nagi.ui

import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.shadow
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.dp
import kotlin.math.hypot
import kotlin.math.max
import kotlin.math.min
import kotlin.math.roundToInt
import kotlin.math.sqrt

// 流量桑基图（见 docs/android-ui.md#图表），和网页概览的「实时流量」同一套布局和动效（web/src/pages/overview.tsx 的 Sankey）。

/** 桑基图的一行：节点、外部网络（[SANKEY_EXTERNAL]）或面板。离线的照常占一行，名称变灰，没有路径。 */
data class SankeyRow(val id: String, val name: String, val offline: Boolean = false)

enum class FlowKind { External, Mesh }

/** 一条路径：from 发出、to 接收，字节/秒。relay 是经面板中转，画成虚线。 */
data class SankeyFlow(val from: String, val to: String, val rate: Double, val kind: FlowKind, val relay: Boolean = false)

/** 显示哪些路径：全部、只看和外部网络之间的、只看节点之间的。 */
enum class SankeyView { All, External, Mesh }

/** 图里的文字（从各页面的字符串资源取）。 */
class SankeyTexts(
    val send: String,
    val receive: String,
    val noSend: String,
    val noReceive: String,
    val offline: String,
    val other: String,
    val relayed: String,
)

/** 外部网络那一行的 ID（和面板给的一样）。 */
const val SANKEY_EXTERNAL = "__external"

private const val OTHER = "__other"

/** 节点超过这个数时只留流量最大的几个，其余合并成「其他」。 */
private const val MAX_NODES = 8

/** 路径断流后留一会儿（显示 0 B/s）再收起：流量小的路径时有时无，否则整张图隔几秒就换一次布局。 */
private const val HOLD_MS = 30_000L

/** 线宽只表示大致大小，准确数字点线看。 */
private fun strokeOf(rate: Double): Float = max(2.0, sqrt(rate / 1024) * 0.24).toFloat()

private fun flowKey(f: SankeyFlow) = "${f.from}|${f.to}|${f.kind}"

/**
 * SankeyChart：左边发出，右边接收，每个节点和外部网络单独一行，两边顺序、高度相同。按实际宽度算布局（单位 dp），不整体缩放。
 * - [flows] 传全部路径，[view] 只决定显示哪些：切换时图不重建，不属于这个视图的路径和行淡出，其余的平滑换位。
 * - 路径增减、速率变化时，行的位置、线的两端和线宽都在 360ms 内过渡（和 AutoHeight 同一条曲线），中途又变了从当前值接着走。
 *   新路径从两端所在的行当前的位置长出来，消失的路径收回所在的行。透明度错开：淡出在前半段走完，淡入在后半段才开始。
 * - 线上不写名称和速率，点一下线在旁边显示「发出 → 接收 速率」，点别处收起。速率跟着刷新，路径没了就不显示。
 * - 系统开了「移除动画」时直接到位。
 */
@Composable
fun SankeyChart(
    rows: List<SankeyRow>,
    flows: List<SankeyFlow>,
    view: SankeyView,
    texts: SankeyTexts,
    modifier: Modifier = Modifier,
    description: String? = null,
) {
    val reduced = reducedMotion()
    val colors = NagiTheme.colors
    val density = LocalDensity.current
    val st = remember { SankeyState() }
    // 淡出的路径和行走完以后加一，让图重新组合、不再画它们。
    var settled by remember { mutableIntStateOf(0) }
    check(settled >= 0)

    // 断流的路径留 HOLD_MS（速率记 0）。在按视图筛选之前算，否则切走的路径会被当成断流留下来。
    val nowMs = System.currentTimeMillis()
    for (f in flows) st.held[flowKey(f)] = f to nowMs
    val live = flows.mapTo(HashSet(), ::flowKey)
    st.held.entries.removeAll { it.key !in live && nowMs - it.value.second > HOLD_MS }
    val all = flows + st.held.entries.filter { it.key !in live }.map { it.value.first.copy(rate = 0.0) }
    val ids = rows.mapTo(HashSet()) { it.id }
    val visible = all.filter { (view == SankeyView.All || it.kind == if (view == SankeyView.External) FlowKind.External else FlowKind.Mesh) && it.from in ids && it.to in ids }
    val (mRows, mFlows) = mergeSmall(rows, visible, texts.other)
    val geo = layout(mRows, mFlows)

    // 正在显示的和正在淡出的路径、行。
    val next = geo.flows
    for ((k, v) in st.shown) if (k !in next) st.leaving[k] = v
    next.keys.forEach { st.leaving.remove(it) }
    st.shown = next
    val nextRows = geo.rows
    for ((k, v) in st.shownRows) if (k !in nextRows) st.leavingRows[k] = v
    nextRows.keys.forEach { st.leavingRows.remove(it) }
    st.shownRows = nextRows

    val goal = HashMap<String, Float>()
    val seeds = HashMap<String, Float>()
    val lastNow = st.values
    for ((k, v) in nextRows) { goal["row:$k"] = v.center; goal["row:$k:op"] = 1f }
    for ((k, v) in st.leavingRows) { goal["row:$k"] = v.center; goal["row:$k:op"] = 0f }
    for ((k, v) in next) {
        goal["$k:y1"] = v.y1; goal["$k:y2"] = v.y2; goal["$k:w"] = strokeOf(v.flow.rate); goal["$k:op"] = 1f
        lastNow["row:${v.flow.from}"]?.let { seeds["$k:y1"] = it }
        lastNow["row:${v.flow.to}"]?.let { seeds["$k:y2"] = it }
    }
    for ((k, v) in st.leaving) {
        goal["$k:y1"] = nextRows[v.flow.from]?.center ?: v.y1
        goal["$k:y2"] = nextRows[v.flow.to]?.center ?: v.y2
        goal["$k:w"] = 0f; goal["$k:op"] = 0f
    }
    goal["h"] = geo.height
    val seedsNow by rememberUpdatedState(seeds)

    LaunchedEffect(goal) {
        val from = st.values
        val s = seedsNow
        if (reduced) {
            st.values = goal
        } else {
            val start = withFrameNanos { it }
            var p: Float
            do {
                val t = withFrameNanos { it }
                p = min(1f, (t - start) / 1_000_000f / Motion.HEIGHT)
                st.values = goal.mapValues { (k, to) ->
                    val fade = k.endsWith(":op")
                    val a = from[k] ?: s[k] ?: if (fade || k.endsWith(":w")) 0f else to
                    val local = when {
                        !fade || a == to -> p
                        to < a -> min(1f, p / 0.5f)
                        else -> max(0f, (p - 0.3f) / 0.7f)
                    }
                    a + (to - a) * Motion.ContentIn.transform(local)
                }
            } while (p < 1f)
        }
        // 淡出的都走完了就不再画，图里只剩当前的。
        if (st.leaving.isNotEmpty() || st.leavingRows.isNotEmpty()) {
            st.leaving.clear()
            st.leavingRows.clear()
            settled++
        }
    }

    val v = st.values
    val drawn = st.shown.entries.toList() + st.leaving.entries.toList()
    val drawnRows = st.shownRows.values.toList() + st.leavingRows.values.toList()
    var selected by remember { mutableStateOf<Pair<String, Offset>?>(null) }
    // 点中的线变实（150ms），其他照常：对外 60%，节点之间 45%。
    val highlight = HashMap<String, Float>()
    for ((k, _) in drawn) key(k) {
        val h by animateFloatAsState(if (selected?.first == k) 1f else 0f, tween(150), label = "flow")
        highlight[k] = h
    }

    BoxWithConstraints(
        modifier.fillMaxWidth().then(if (description != null) Modifier.semantics { contentDescription = description } else Modifier),
    ) {
        val width = maxWidth.value
        val label = min(128f, (width * 0.24f).roundToInt().toFloat())
        val x0 = label
        val x3 = width - label
        val x1 = x0 + min(176f, (x3 - x0) * 0.3f)
        val x2 = x3 - min(120f, (x3 - x0) * 0.2f)
        val cx = (x1 + x2) / 2
        val nameMax = max(label - 8, 40f)
        val height = v["h"] ?: geo.height

        val hit by rememberUpdatedState { at: Offset ->
            // 细线不好点中：离线比线宽两边各多 8，至少 10。
            var best: String? = null
            var bestD = Float.MAX_VALUE
            for ((k, d) in st.shown) {
                if ((st.values["$k:op"] ?: 0f) < 0.5f) continue
                val y1 = st.values["$k:y1"] ?: d.y1
                val y2 = st.values["$k:y2"] ?: d.y2
                val w = st.values["$k:w"] ?: 0f
                val dist = distanceToFlow(at, x0, x1, x2, x3, cx, y1, y2)
                if (dist <= max(w / 2 + 8, 10f) && dist < bestD) { best = k; bestD = dist }
            }
            selected = best?.let { it to at }
        }

        Canvas(
            Modifier.fillMaxWidth().height(height.dp).pointerInput(Unit) {
                detectTapGestures { pos -> hit(Offset(pos.x / density.density, pos.y / density.density)) }
            },
        ) {
            val s = density.density
            for ((k, d) in drawn) {
                val op = v["$k:op"] ?: 0f
                if (op <= 0f) continue
                val w = v["$k:w"] ?: 0f
                val y1 = v["$k:y1"] ?: d.y1
                val y2 = v["$k:y2"] ?: d.y2
                val path = Path().apply {
                    moveTo(x0 * s, y1 * s); lineTo(x1 * s, y1 * s); cubicTo(cx * s, y1 * s, cx * s, y2 * s, x2 * s, y2 * s); lineTo(x3 * s, y2 * s)
                }
                val base = if (d.flow.kind == FlowKind.External) 0.6f else 0.45f
                val alpha = op * (base + (1f - base) * (highlight[k] ?: 0f))
                val color = if (d.flow.kind == FlowKind.External) colors.brand else colors.mutedForeground
                drawPath(
                    path, color.copy(alpha = alpha),
                    style = Stroke(
                        width = w * s, cap = StrokeCap.Butt,
                        pathEffect = if (d.flow.relay) PathEffect.dashPathEffect(floatArrayOf(max(w, 4f) * s, 4f * s)) else null,
                    ),
                )
            }
        }
        // 两边的标题。
        Text(texts.send, style = NagiTheme.type.small, color = colors.mutedForeground)
        Text(texts.receive, style = NagiTheme.type.small, color = colors.mutedForeground, modifier = Modifier.align(Alignment.TopEnd))
        // 名称和速率：名称在线的上方，速率在下方，两边各一份。速率变了逐位滚动。
        for (r in drawnRows) key(r.id) {
            val center = v["row:${r.id}"] ?: r.center
            val op = v["row:${r.id}:op"] ?: 0f
            val nameColor = if (r.offline) colors.mutedForeground else colors.foreground
            val sendText = if (r.offline) texts.offline else if (r.send > 0) formatRateText(r.send) else texts.noSend
            val receiveText = if (r.offline) texts.offline else if (r.receive > 0) formatRateText(r.receive) else texts.noReceive
            for (end in listOf(false, true)) {
                Column(
                    Modifier.align(if (end) Alignment.TopEnd else Alignment.TopStart)
                        .offset { IntOffset(0, ((center - 24f) * density.density).roundToInt()) }
                        .graphicsLayer { alpha = op },
                    horizontalAlignment = if (end) Alignment.End else Alignment.Start,
                ) {
                    Text(
                        r.name, style = NagiTheme.type.body, color = nameColor, maxLines = 1, overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.widthIn(max = nameMax.dp),
                    )
                    Roll(if (end) receiveText else sendText, NagiTheme.type.small, colors.mutedForeground)
                }
            }
        }
        // 点中的线：提示放在点的右上方，靠右时放到左边，免得超出图。
        val sel = selected
        val flow = sel?.let { st.shown[it.first]?.flow }
        if (sel != null && flow != null) {
            val nameOf = { id: String -> mRows.firstOrNull { it.id == id }?.name ?: id }
            val at = sel.second
            val toLeft = at.x > width - 200
            Column(
                Modifier.align(if (toLeft) Alignment.TopEnd else Alignment.TopStart)
                    .offset { IntOffset(((if (toLeft) -(width - at.x + 12) else at.x + 12) * density.density).roundToInt(), (max(0f, at.y - 44) * density.density).roundToInt()) }
                    .shadow(8.dp, RoundedCornerShape(8.dp))
                    .background(colors.card, RoundedCornerShape(8.dp))
                    .padding(horizontal = 10.dp, vertical = 6.dp),
            ) {
                Text("${nameOf(flow.from)} → ${nameOf(flow.to)}", style = NagiTheme.type.small, color = colors.foreground)
                Row {
                    Roll(formatRateText(flow.rate), NagiTheme.type.small, colors.mutedForeground)
                    if (flow.relay) Text(" · ${texts.relayed}", style = NagiTheme.type.small, color = colors.mutedForeground)
                }
            }
        }
    }
}

private class DrawnRow(val id: String, val name: String, val offline: Boolean, val center: Float, val send: Double, val receive: Double)

private class DrawnFlow(val flow: SankeyFlow, val y1: Float, val y2: Float)

private class Geometry(val rows: LinkedHashMap<String, DrawnRow>, val flows: LinkedHashMap<String, DrawnFlow>, val height: Float)

private class SankeyState {
    var shown = LinkedHashMap<String, DrawnFlow>()
    val leaving = LinkedHashMap<String, DrawnFlow>()
    var shownRows = LinkedHashMap<String, DrawnRow>()
    val leavingRows = LinkedHashMap<String, DrawnRow>()
    var values by mutableStateOf<Map<String, Float>>(emptyMap())
    val held = HashMap<String, Pair<SankeyFlow, Long>>()
}

/** 节点太多时，流量小的节点合并成「其他」。 */
private fun mergeSmall(rows: List<SankeyRow>, flows: List<SankeyFlow>, otherName: String): Pair<List<SankeyRow>, List<SankeyFlow>> {
    val nodes = rows.filter { it.id != SANKEY_EXTERNAL }
    if (nodes.size <= MAX_NODES) return rows to flows
    val total = HashMap<String, Double>()
    for (f in flows) {
        total[f.from] = (total[f.from] ?: 0.0) + f.rate
        total[f.to] = (total[f.to] ?: 0.0) + f.rate
    }
    val keep = nodes.sortedByDescending { total[it.id] ?: 0.0 }.take(MAX_NODES - 1).mapTo(HashSet()) { it.id }
    fun pick(id: String) = if (id == SANKEY_EXTERNAL || id in keep) id else OTHER
    val merged = LinkedHashMap<String, SankeyFlow>()
    for (f in flows) {
        val k = "${pick(f.from)}|${pick(f.to)}|${f.kind}"
        val old = merged[k]
        merged[k] = old?.copy(rate = old.rate + f.rate) ?: f.copy(from = pick(f.from), to = pick(f.to))
    }
    return (rows.filter { it.id == SANKEY_EXTERNAL || it.id in keep } + SankeyRow(OTHER, otherName)) to merged.values.toList()
}

/** 先排每一行，再按行算每条线两端的位置。一行的高度取两边较高的一边。两边的线都靠近名称那一行，比中心高 7。 */
private fun layout(rows: List<SankeyRow>, flows: List<SankeyFlow>): Geometry {
    val order = rows.withIndex().associate { it.value.id to it.index }
    val gap = 4f
    val groupGap = 42f
    fun stack(list: List<SankeyFlow>) = if (list.isEmpty()) 2f else list.sumOf { strokeOf(it.rate).toDouble() }.toFloat() + (list.size - 1) * gap
    var y = 50f
    val outRows = LinkedHashMap<String, DrawnRow>()
    val starts = HashMap<String, Float>()
    val ends = HashMap<String, Float>()
    for (r in rows) {
        val out = flows.filter { it.from == r.id }.sortedBy { order[it.to] ?: 0 }
        val into = flows.filter { it.to == r.id }.sortedBy { order[it.from] ?: 0 }
        val left = stack(out)
        val right = stack(into)
        val rowHeight = max(left, right)
        val center = y + rowHeight / 2
        y += rowHeight + groupGap
        var py = center - 7 - left / 2
        for (f in out) {
            starts[flowKey(f)] = py + strokeOf(f.rate) / 2
            py += strokeOf(f.rate) + gap
        }
        var ry = center - 7 - right / 2
        for (f in into) {
            ends[flowKey(f)] = ry + strokeOf(f.rate) / 2
            ry += strokeOf(f.rate) + gap
        }
        outRows[r.id] = DrawnRow(r.id, r.name, r.offline, center, out.sumOf { it.rate }, into.sumOf { it.rate })
    }
    val outFlows = LinkedHashMap<String, DrawnFlow>()
    for (f in flows) {
        val k = flowKey(f)
        outFlows[k] = DrawnFlow(f, starts[k] ?: continue, ends[k] ?: continue)
    }
    // 最后一行可能有多条线，比名称那两行更低，取较低的一边，否则最下面的速率会被裁掉。
    val height = max((outRows.values.lastOrNull()?.center ?: 0f) + 13, y - groupGap + 2) + 12
    return Geometry(outRows, outFlows, height)
}

/** 点到一条路径的距离（dp）：两段直线加中间的三次曲线（取 24 个点连成折线）。 */
private fun distanceToFlow(p: Offset, x0: Float, x1: Float, x2: Float, x3: Float, cx: Float, y1: Float, y2: Float): Float {
    var best = min(segment(p, x0, y1, x1, y1), segment(p, x2, y2, x3, y2))
    var px = x1
    var py = y1
    for (i in 1..24) {
        val t = i / 24f
        val u = 1 - t
        val x = u * u * u * x1 + 3 * u * u * t * cx + 3 * u * t * t * cx + t * t * t * x2
        val y = u * u * u * y1 + 3 * u * u * t * y1 + 3 * u * t * t * y2 + t * t * t * y2
        best = min(best, segment(p, px, py, x, y))
        px = x
        py = y
    }
    return best
}

private fun segment(p: Offset, ax: Float, ay: Float, bx: Float, by: Float): Float {
    val dx = bx - ax
    val dy = by - ay
    val len = dx * dx + dy * dy
    val t = if (len == 0f) 0f else (((p.x - ax) * dx + (p.y - ay) * dy) / len).coerceIn(0f, 1f)
    return hypot(p.x - (ax + t * dx), p.y - (ay + t * dy))
}
