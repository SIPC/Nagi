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
import androidx.compose.ui.graphics.nativeCanvas
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.layout.layout
import androidx.compose.ui.platform.LocalConfiguration
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

/**
 * 尺寸（dp）：网页的取值，和手机上收紧的一套。手机的内容区只有 320 左右，照搬网页的行距和线宽显得松、线太粗；
 * 宽度达到 600（平板、横屏）时用网页的。
 */
/**
 * 一行的高度取线叠起来的高度和名称、速率两行字的高度（[minRow]）中较大的，行和行之间再留 [groupGap]：
 * 节点多时一行有很多条细线，只按线算会比名称高出一截，行距就显得很大。线和线之间留 [lineGap]。
 */
private class SankeySpec(
    val compact: Boolean,
    val strokeScale: Double,
    val strokeMax: Double,
    val groupGap: Float,
    val lineGap: Float,
    val minRow: Float,
    val top: Float,
    val labelRatio: Float,
    val nameTop: Float,
) {
    /** 收紧一档：行和行之间少 [step]（最少 6），一行的最小高度跟着少（最少 34，刚好放下名称和速率两行字）。 */
    fun tighter(step: Float) = SankeySpec(
        compact, strokeScale, strokeMax, groupGap = maxOf(6f, groupGap - step), lineGap = lineGap, minRow = maxOf(34f, minRow - step),
        top = top, labelRatio = labelRatio, nameTop = nameTop,
    )

    val tightest: Boolean get() = groupGap <= 6f && minRow <= 34f

    /** 放宽：行和行之间多 [add]。 */
    fun looser(add: Float) = SankeySpec(
        compact, strokeScale, strokeMax, groupGap = groupGap + maxOf(0f, add), lineGap = lineGap, minRow = minRow,
        top = top, labelRatio = labelRatio, nameTop = nameTop,
    )
}

private val WideSpec = SankeySpec(compact = false, strokeScale = 0.24, strokeMax = Double.MAX_VALUE, groupGap = 24f, lineGap = 2f, minRow = 40f, top = 50f, labelRatio = 0.24f, nameTop = 24f)
private val CompactSpec = SankeySpec(compact = true, strokeScale = 0.18, strokeMax = 20.0, groupGap = 12f, lineGap = 2f, minRow = 36f, top = 40f, labelRatio = 0.28f, nameTop = 22f)

/** 线宽的上限（dp）：网页的取值不限线宽，速率大得离谱时（例如节点乱报）也不让一条线高到画不出来。 */
private const val STROKE_CAP = 400.0

/** 线宽只表示大致大小，准确数字点线看。速率不是有限的正数时按最细的画（v0.35.0#16）。 */
private fun SankeySpec.strokeOf(rate: Double): Float =
    if (!rate.isFinite() || rate <= 0) 2f else max(2.0, min(min(strokeMax, STROKE_CAP), sqrt(rate / 1024) * strokeScale)).toFloat()

/** 速率来自节点上报，可能是 NaN、Inf 或负数：NaN、Inf 的路径不画，负数当 0（v0.35.0#16）。 */
internal fun finiteFlows(flows: List<SankeyFlow>): List<SankeyFlow> =
    flows.filter { it.rate.isFinite() }.map { if (it.rate < 0) it.copy(rate = 0.0) else it }

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
    maxHeight: androidx.compose.ui.unit.Dp? = null,
    fillHeight: Boolean = false,
) {
    val reduced = reducedMotion()
    val colors = NagiTheme.colors
    val density = LocalDensity.current
    val st = remember { SankeyState() }
    // 量名称和速率的宽度：两边各十来行，默认的缓存（8 条）不够，每次重组都要重新排版。
    val measurer = androidx.compose.ui.text.rememberTextMeasurer(cacheSize = 64)
    // 淡出的路径和行走完以后加一，让图重新组合、不再画它们。
    var settled by remember { mutableIntStateOf(0) }
    check(settled >= 0)

    @Suppress("NAME_SHADOWING") val flows = finiteFlows(flows)
    // 断流的路径留 HOLD_MS（速率记 0）。在按视图筛选之前算，否则切走的路径会被当成断流留下来。
    val nowMs = System.currentTimeMillis()
    for (f in flows) st.held[flowKey(f)] = f to nowMs
    val live = flows.mapTo(HashSet(), ::flowKey)
    st.held.entries.removeAll { it.key !in live && nowMs - it.value.second > HOLD_MS }
    val all = flows + st.held.entries.filter { it.key !in live }.map { it.value.first.copy(rate = 0.0) }
    val ids = rows.mapTo(HashSet()) { it.id }
    val visible = all.filter { (view == SankeyView.All || it.kind == if (view == SankeyView.External) FlowKind.External else FlowKind.Mesh) && it.from in ids && it.to in ids }
    // 高度自适应：节点少时照常；超过 [maxHeight] 时一档一档收紧行距（每次 2），刚好放下就停；
    // 收到最紧还放不下，才少列几个节点，流量小的合并成「其他」（至少留 2 个）。
    val base = if (LocalConfiguration.current.screenWidthDp < 600) CompactSpec else WideSpec
    var spec = base
    var maxNodes = MAX_NODES
    var merged = mergeSmall(rows, visible, texts.other, maxNodes)
    var geo = spec.layout(merged.first, merged.second)
    val cap = maxHeight?.value
    if (cap != null && geo.height > cap) {
        while (geo.height > cap && !spec.tightest) {
            spec = spec.tighter(2f)
            geo = spec.layout(merged.first, merged.second)
        }
        while (geo.height > cap && maxNodes > 2) {
            maxNodes--
            merged = mergeSmall(rows, visible, texts.other, maxNodes)
            geo = spec.layout(merged.first, merged.second)
        }
    }
    // [fillHeight]：比 [maxHeight] 矮时把行距加大，铺满这个高度（行和行之间最多 72，节点少时不拉得太散）。
    if (fillHeight && cap != null && geo.height < cap && merged.first.size > 1) {
        val more = (cap - geo.height) / (merged.first.size - 1)
        spec = spec.looser(minOf(more, 72f - spec.groupGap))
        geo = spec.layout(merged.first, merged.second)
    }
    val (mRows, mFlows) = merged

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
        goal["$k:y1"] = v.y1; goal["$k:y2"] = v.y2; goal["$k:w"] = spec.strokeOf(v.flow.rate); goal["$k:op"] = 1f
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
        // 变化小到看不出（位置、线宽都不到 0.5，透明度没变）时直接到位：每 10 秒刷新一次，速率通常只差一点，
        // 不必为此把几十条线重画 360ms。
        val small = goal.all { (k, to) ->
            val a = from[k] ?: s[k] ?: return@all false
            kotlin.math.abs(to - a) < if (k.endsWith(":op")) 0.01f else 0.5f
        }
        if (reduced || small) {
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

    // st.values 每一帧都在变，只在绘制和布局阶段读（Canvas、offset、graphicsLayer、layout），动画时不重组整张图。
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
        // 两边留给名称和速率的宽度按实际的文字量：最宽的那一项再加 12，线从这里开始，最多占宽度的 labelRatio。
        // 速率一直在变，留的宽度只增不减，免得整张图跟着数字左右抖。
        val cap = (width * spec.labelRatio).roundToInt().toFloat()
        fun widest(texts: List<String>, style: androidx.compose.ui.text.TextStyle) =
            texts.maxOfOrNull { with(density) { measurer.measure(it, style, maxLines = 1).size.width.toDp().value } } ?: 0f
        val nameStyle = if (spec.compact) NagiTheme.type.secondary else NagiTheme.type.body
        val names = drawnRows.map { it.name }
        val leftNeed = max(widest(names, nameStyle), widest(drawnRows.map { if (it.offline) texts.offline else if (it.send > 0) formatRateText(it.send) else texts.noSend }, NagiTheme.type.small)) + 12
        val rightNeed = max(widest(names, nameStyle), widest(drawnRows.map { if (it.offline) texts.offline else if (it.receive > 0) formatRateText(it.receive) else texts.noReceive }, NagiTheme.type.small)) + 12
        st.leftLabel = max(st.leftLabel, min(cap, leftNeed))
        st.rightLabel = max(st.rightLabel, min(cap, rightNeed))
        val x0 = st.leftLabel
        val x3 = width - st.rightLabel
        val x1 = x0 + min(176f, (x3 - x0) * 0.3f)
        val x2 = x3 - min(120f, (x3 - x0) * 0.2f)
        val cx = (x1 + x2) / 2
        val nameMax = max(min(st.leftLabel, st.rightLabel) - 8, 40f)
        val fallbackHeight = geo.height

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
            // 手指点一下看那条线；接鼠标、触控板或用触控笔时悬浮就看，移出收起。
            Modifier.fillMaxWidth()
                .layout { m, c ->
                    val h = ((st.values["h"] ?: fallbackHeight) * density.density).roundToInt()
                    val p = m.measure(c.copy(minHeight = h, maxHeight = h))
                    layout(p.width, h) { p.place(0, 0) }
                }
                .mouseHover(onMove = { pos -> hit(Offset(pos.x / density.density, pos.y / density.density)) }, onExit = { selected = null })
                .pointerInput(Unit) {
                    detectTapGestures { pos -> hit(Offset(pos.x / density.density, pos.y / density.density)) }
                },
        ) {
            val s = density.density
            val v = st.values
            val native = drawContext.canvas.nativeCanvas
            for ((k, d) in drawn) {
                val op = v["$k:op"] ?: 0f
                if (op <= 0f) continue
                val w = v["$k:w"] ?: 0f
                val y1 = v["$k:y1"] ?: d.y1
                val y2 = v["$k:y2"] ?: d.y2
                val base = if (d.flow.kind == FlowKind.External) 0.6f else 0.45f
                val alpha = op * (base + (1f - base) * (highlight[k] ?: 0f))
                val color = if (d.flow.kind == FlowKind.External) colors.brand else colors.mutedForeground
                if (d.flow.relay) {
                    // 经面板中转的画成虚线，很少见，照常描边。
                    val path = Path().apply {
                        moveTo(x0 * s, y1 * s); lineTo(x1 * s, y1 * s); cubicTo(cx * s, y1 * s, cx * s, y2 * s, x2 * s, y2 * s); lineTo(x3 * s, y2 * s)
                    }
                    drawPath(
                        path, color.copy(alpha = alpha),
                        style = Stroke(width = w * s, cap = StrokeCap.Butt, pathEffect = PathEffect.dashPathEffect(floatArrayOf(max(w, 4f) * s, 4f * s))),
                    )
                } else {
                    st.band.draw(native, x0 * s, x1 * s, x2 * s, x3 * s, cx * s, y1 * s, y2 * s, w * s, color.copy(alpha = alpha).toArgb())
                }
            }
        }
        // 两边的标题。
        Text(texts.send, style = NagiTheme.type.small, color = colors.mutedForeground)
        Text(texts.receive, style = NagiTheme.type.small, color = colors.mutedForeground, modifier = Modifier.align(Alignment.TopEnd))
        // 名称和速率：名称在线的上方，速率在下方，两边各一份。速率变了逐位滚动。
        for (r in drawnRows) key(r.id) {
            val centerKey = "row:${r.id}"
            val opKey = "row:${r.id}:op"
            val nameColor = if (r.offline) colors.mutedForeground else colors.foreground
            val sendText = if (r.offline) texts.offline else if (r.send > 0) formatRateText(r.send) else texts.noSend
            val receiveText = if (r.offline) texts.offline else if (r.receive > 0) formatRateText(r.receive) else texts.noReceive
            for (end in listOf(false, true)) {
                Column(
                    Modifier.align(if (end) Alignment.TopEnd else Alignment.TopStart)
                        .offset { IntOffset(0, (((st.values[centerKey] ?: r.center) - spec.nameTop) * density.density).roundToInt()) }
                        .graphicsLayer { alpha = st.values[opKey] ?: 0f },
                    horizontalAlignment = if (end) Alignment.End else Alignment.Start,
                ) {
                    Text(
                        r.name, style = if (spec.compact) NagiTheme.type.secondary else NagiTheme.type.body, color = nameColor, maxLines = 1, overflow = TextOverflow.Ellipsis,
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

/**
 * 一条路径画成三角形网格，交给 GPU 直接画：带宽度的曲线描边要在 CPU 上算轮廓、光栅化成遮罩再上传，
 * 几十条线一起过渡时每帧要十几毫秒。中线是「直线、三次曲线、直线」，沿中线采样，两边按法线各偏半个线宽，
 * 最外面再留 1px 透明度渐变做抗锯齿，看起来和描边一样。缓冲区复用，不每帧分配。
 */
internal class BandMesh {
    private var verts = FloatArray(0)
    private var colors = IntArray(0)
    private var indices = ShortArray(0)
    private val paint = android.graphics.Paint()

    fun draw(canvas: android.graphics.Canvas, x0: Float, x1: Float, x2: Float, x3: Float, cx: Float, y1: Float, y2: Float, width: Float, argb: Int) {
        val dy = kotlin.math.abs(y2 - y1)
        // 三次曲线分几段：按长度大约每 12px 一段，至少 8 段、最多 64 段；两头的直线各一段。
        val n = if (dy < 0.5f) 1 else ((dy + (x2 - x1)) / 12f).toInt().coerceIn(8, 64)
        val points = n + 3
        if (verts.size < points * 8) {
            verts = FloatArray(points * 8)
            colors = IntArray(points * 4)
            indices = ShortArray((points - 1) * 18)
        }
        val half = width / 2
        val outer = half + 0.5f
        val inner = max(0f, half - 0.5f)
        val clear = argb and 0x00FFFFFF
        fun put(i: Int, px: Float, py: Float, nx: Float, ny: Float) {
            val o = i * 8
            verts[o] = px - nx * outer; verts[o + 1] = py - ny * outer
            verts[o + 2] = px - nx * inner; verts[o + 3] = py - ny * inner
            verts[o + 4] = px + nx * inner; verts[o + 5] = py + ny * inner
            verts[o + 6] = px + nx * outer; verts[o + 7] = py + ny * outer
            val c = i * 4
            colors[c] = clear; colors[c + 1] = argb; colors[c + 2] = argb; colors[c + 3] = clear
        }
        put(0, x0, y1, 0f, 1f)
        for (j in 0..n) {
            val t = j.toFloat() / n
            val u = 1 - t
            // B(t)，控制点 (x1,y1) (cx,y1) (cx,y2) (x2,y2)；切线的 x 分量总是正的，法线方向一致。
            val px = u * u * u * x1 + 3 * u * u * t * cx + 3 * u * t * t * cx + t * t * t * x2
            val py = u * u * u * y1 + 3 * u * u * t * y1 + 3 * u * t * t * y2 + t * t * t * y2
            val tx = 3 * u * u * (cx - x1) + 3 * t * t * (x2 - cx)
            val ty = 6 * u * t * (y2 - y1)
            val len = hypot(tx, ty)
            if (len < 1e-3f) put(j + 1, px, py, 0f, 1f) else put(j + 1, px, py, -ty / len, tx / len)
        }
        put(points - 1, x3, y2, 0f, 1f)
        var k = 0
        for (i in 0 until points - 1) {
            val a = i * 4
            val b = a + 4
            for (e in 0 until 3) {
                indices[k++] = (a + e).toShort(); indices[k++] = (b + e).toShort(); indices[k++] = (a + e + 1).toShort()
                indices[k++] = (a + e + 1).toShort(); indices[k++] = (b + e).toShort(); indices[k++] = (b + e + 1).toShort()
            }
        }
        canvas.drawVertices(android.graphics.Canvas.VertexMode.TRIANGLES, points * 8, verts, 0, null, 0, colors, 0, indices, 0, k, paint)
    }
}

private class SankeyState {
    val band = BandMesh()
    var shown = LinkedHashMap<String, DrawnFlow>()
    val leaving = LinkedHashMap<String, DrawnFlow>()
    var shownRows = LinkedHashMap<String, DrawnRow>()
    val leavingRows = LinkedHashMap<String, DrawnRow>()
    var values by mutableStateOf<Map<String, Float>>(emptyMap())
    val held = HashMap<String, Pair<SankeyFlow, Long>>()
    /** 两边留给文字的宽度（dp），只增不减。 */
    var leftLabel = 0f
    var rightLabel = 0f
}

/** 节点太多时，流量小的节点合并成「其他」。 */
private fun mergeSmall(rows: List<SankeyRow>, flows: List<SankeyFlow>, otherName: String, maxNodes: Int): Pair<List<SankeyRow>, List<SankeyFlow>> {
    val nodes = rows.filter { it.id != SANKEY_EXTERNAL }
    if (nodes.size <= maxNodes) return rows to flows
    val total = HashMap<String, Double>()
    for (f in flows) {
        total[f.from] = (total[f.from] ?: 0.0) + f.rate
        total[f.to] = (total[f.to] ?: 0.0) + f.rate
    }
    val keep = nodes.sortedByDescending { total[it.id] ?: 0.0 }.take(maxNodes - 1).mapTo(HashSet()) { it.id }
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
private fun SankeySpec.layout(rows: List<SankeyRow>, flows: List<SankeyFlow>): Geometry {
    val order = rows.withIndex().associate { it.value.id to it.index }
    val gap = lineGap
    fun stack(list: List<SankeyFlow>) = if (list.isEmpty()) 2f else list.sumOf { strokeOf(it.rate).toDouble() }.toFloat() + (list.size - 1) * gap
    var y = top
    val outRows = LinkedHashMap<String, DrawnRow>()
    val starts = HashMap<String, Float>()
    val ends = HashMap<String, Float>()
    for (r in rows) {
        val out = flows.filter { it.from == r.id }.sortedBy { order[it.to] ?: 0 }
        val into = flows.filter { it.to == r.id }.sortedBy { order[it.from] ?: 0 }
        val left = stack(out)
        val right = stack(into)
        val rowHeight = maxOf(left, right, minRow)
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
    // 最后一行的速率在 center + 12 处结束（名称和速率两行字），图到这里为止，底部和旁边的内容对齐。
    val height = max((outRows.values.lastOrNull()?.center ?: 0f) + 13, y - groupGap + 2)
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
