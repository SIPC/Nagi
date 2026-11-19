package ink.sipc.nagi.ui

import androidx.compose.animation.core.animateDpAsState
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.snap
import androidx.compose.foundation.background
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.collectIsPressedAsState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.wrapContentWidth
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Rect
import androidx.compose.ui.geometry.RoundRect
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.PathOperation
import androidx.compose.ui.graphics.TransformOrigin
import androidx.compose.ui.graphics.asAndroidPath
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.drawIntoCanvas
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.graphics.nativeCanvas
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.LayoutCoordinates
import androidx.compose.ui.layout.onPlaced
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.dp
import kotlin.math.roundToInt

data class PillNavItem(val icon: ImageVector, val label: String, val badge: Boolean = false)

/** 胶囊导航的方向：横向放在底部（手机），竖向放在左栏（宽屏）。 */
enum class PillOrientation { Horizontal, Vertical }

// PillNav：悬浮的胶囊导航（docs/android.md 导航、宽屏）。横向和竖向是同一个组件，外框、选中底色、长按拖动和动效都共用：
// - 外框：底色（pillBackground）、1px 边框、浅阴影。长按时以中心放大 4%，松手回弹（PillGestures.kt）。
// - 选中底色从上一项滑过去（SlidingIndicator：朝移动方向的边快、另一条边慢，带一点回弹），后面跟一小段拖尾。
//   回弹越出内容区时，外框在那一端按越出的距离撑开（最多 FRAME_MAX），回弹结束一起收回；中间几项的回弹在框内，外框不动。
//   竖向时上面是 header（logo），底色往上回弹还在内容区里，顶部自然不撑开。
// - 长按后沿胶囊拖动：手指经过哪一项就切到哪一项，每切一次轻振一下。手指下是哪一项按项目区等分来算。
// 两个方向的区别只在名称怎么显示：
// - 横向：每一项占固定的位置，没选中的只有图标，选中的一项按最长的名称留宽度，名称在里面居中。
//   切换时旧的一项收起、新的一项展开，用同一条弹簧，一减一加正好抵消，胶囊的总宽度不变，不会跳。
// - 竖向：只有图标。拖动时选中的那一项横着展开：底色往右伸长、图标右边淡入名称，外框在那一行跟着鼓出来，
//   和胶囊连成一个形状（接缝处用圆弧过渡），松手后用弹簧收回。展开的部分盖在右边的内容上面，胶囊要放在内容的上面一层（zIndex）。
// 系统开了「移除动画」时都直接到位。badge 是图标右上角的红点（有严重告警时）。
// 组件只画胶囊本身，放在哪里（底部居中、离手势条多远）由调用方决定。

/** 底色回弹越出两端时，外框撑开的距离是越出距离的几倍，以及最多撑开多少。 */
private const val FRAME_FOLLOW = 1f
private val FRAME_MAX = 6.dp

/** 选中底色的拖尾最长多少。 */
private val TAIL_MAX = 20.dp

/** 外框和每一项之间的内边距。 */
private val PillPadding = 4.dp
private val IconSize = 22.dp

// 横向：胶囊高 52（Dimens.pillHeight），每一项高 44、左右各 13，图标和名称之间 4。
private val ItemPadding = 13.dp
private val IconLabelGap = 4.dp

// 竖向：宽 64，每一项 56 见方，项和项之间 4。拖动时名称从图标右边 9 处开始，名称右边留 14
// （比图标左边的留白窄 3，名称在图标右边的空当里看起来居中）。
private val RailItem = 56.dp
private val RailGap = 4.dp
private val LabelGap = 9.dp
private val LabelEnd = 14.dp

/** 竖向展开时鼓出的部分和胶囊接缝处的圆弧半径。 */
private val BulgeFillet = 14.dp

/** 胶囊导航的底色：深色时比 card（#171717）深一档，用 #121212；浅色时照常用 card。 */
internal fun pillBackground(c: NagiColors, dark: Boolean) = if (dark) Color(0xFF121212) else c.card

/** 各项相对胶囊外框的位置：外框和每一项放好时各自记下坐标，换算到外框里。 */
private class ItemBounds {
    var frame: LayoutCoordinates? = null
    val items = HashMap<Int, LayoutCoordinates>()
    val rects = mutableStateMapOf<Int, Rect>()

    fun update(i: Int) {
        val f = frame ?: return
        val c = items[i] ?: return
        if (f.isAttached && c.isAttached) rects[i] = f.localBoundingBoxOf(c, clipBounds = false)
    }
}

/**
 * 胶囊导航。[orientation] 选横向（底部）或竖向（宽屏左栏）；[header] 放在各项前面，不可点、不参与选择（竖向时是 logo）。
 */
@Composable
fun PillNav(
    items: List<PillNavItem>,
    selected: Int,
    onSelect: (Int) -> Unit,
    modifier: Modifier = Modifier,
    orientation: PillOrientation = PillOrientation.Horizontal,
    header: (@Composable () -> Unit)? = null,
) {
    val c = NagiTheme.colors
    val dark = LocalNagiDark.current
    val reduced = reducedMotion()
    val density = LocalDensity.current
    val vertical = orientation == PillOrientation.Vertical
    val labelStyle = NagiTheme.type.bodyMedium
    val measurer = rememberTextMeasurer()

    // 横向：最长的名称宽度，选中的一项按它留位置，换到哪一项胶囊都一样宽。
    val maxLabel = remember(items, labelStyle) {
        with(density) { items.maxOf { measurer.measure(it.label, labelStyle, maxLines = 1).size.width }.toDp() }
    }

    val drag = rememberPillDrag()
    val scale = drag.scale()
    val bounds = remember { ItemBounds() }
    val slider = rememberSlidingIndicator(selected)

    // 竖向拖动时的展开进度：0 只有图标，1 底色伸长、名称完整显示。和横向每一项的展开同一条弹簧。
    val open by animateFloatAsState(if (vertical && drag.dragging && !reduced) 1f else 0f, if (reduced) snap() else Motion.itemSpring(), label = "open")
    // 展开后多出来的宽度：名称从图标右边 LabelGap 处开始，右边留 LabelEnd，减去那一项原有的宽度。名称变了宽度跟着过渡。
    val label = items.getOrNull(selected)?.label.orEmpty()
    val labelWidth = with(density) { measurer.measure(label, labelStyle, maxLines = 1).size.width.toDp() }
    val iconInset = (RailItem - IconSize) / 2
    val extra by animateDpAsState(iconInset + IconSize + LabelGap + labelWidth + LabelEnd - RailItem, if (reduced) snap() else Motion.itemSpring(), label = "extra")

    val shadowColor = if (dark) 0x80000000.toInt() else 0x14000000
    val paint = remember { android.graphics.Paint().apply { isAntiAlias = true } }

    PillLongPress {
        Box(
            modifier
                .graphicsLayer { scaleX = scale; scaleY = scale; transformOrigin = TransformOrigin.Center }
                .onPlaced { bounds.frame = it; bounds.items.keys.forEach(bounds::update) }
                .drawBehind {
                    val r = slider.rect(bounds.rects, vertical)
                    val pad = PillPadding.toPx()
                    val maxOver = FRAME_MAX.toPx()
                    fun over(d: Float) = (maxOf(0f, d) * FRAME_FOLLOW).coerceAtMost(maxOver)
                    // 外框：胶囊本身，底色越出内容区时那一端撑开。
                    var left = 0f
                    var top = 0f
                    var right = size.width
                    var bottom = size.height
                    if (r != null) {
                        if (vertical) {
                            top -= over(pad - r.top)
                            bottom += over(r.bottom - (size.height - pad))
                        } else {
                            left -= over(pad - r.left)
                            right += over(r.right - (size.width - pad))
                        }
                    }
                    val frame = Path().apply { addRoundRect(RoundRect(left, top, right, bottom, CornerRadius(minOf(size.width, size.height) / 2))) }
                    // 竖向展开：那一行往右鼓出（比那一项上下各大 PillPadding），和胶囊合成一个形状，接缝处用圆弧过渡。
                    val grow = if (vertical && r != null) extra.toPx() * open else 0f
                    if (grow > 0.5f && r != null) {
                        val bt = r.top - pad
                        val bb = r.bottom + pad
                        val cap = size.width / 2
                        frame.op(frame, Path().apply { addRoundRect(RoundRect(0f, bt, size.width + grow, bb, CornerRadius(cap))) }, PathOperation.Union)
                        for (upper in listOf(true, false)) {
                            bulgeFillet(size.width, top, bottom, bt, bb, size.width + grow, cap, BulgeFillet.toPx(), upper)
                                ?.let { frame.op(frame, it, PathOperation.Union) }
                        }
                    }
                    drawIntoCanvas { canvas ->
                        paint.color = pillBackground(c, dark).toArgb()
                        paint.setShadowLayer(16.dp.toPx(), 0f, 4.dp.toPx(), shadowColor)
                        canvas.nativeCanvas.drawPath(frame.asAndroidPath(), paint)
                    }
                    drawPath(frame, c.border, style = Stroke(1.dp.toPx()))
                    if (r == null) return@drawBehind
                    val indicator = c.muted
                    val cross = if (vertical) r.width else r.height
                    // 拖尾：底色后面一小段（最长 TAIL_MAX），从底色渐变到透明，滑到位时缩回。
                    slider.tail(bounds.rects, r, TAIL_MAX.toPx(), vertical)?.let { (from, to) ->
                        if (vertical) {
                            val fade = from < r.top
                            drawRoundRect(
                                Brush.verticalGradient(if (fade) listOf(indicator.copy(alpha = 0f), indicator) else listOf(indicator, indicator.copy(alpha = 0f)), startY = from, endY = to),
                                Offset(r.left + cross * 0.15f, from), Size(cross * 0.7f, to - from), CornerRadius(cross * 0.35f),
                            )
                        } else {
                            val fade = from < r.left
                            drawRoundRect(
                                Brush.horizontalGradient(if (fade) listOf(indicator.copy(alpha = 0f), indicator) else listOf(indicator, indicator.copy(alpha = 0f)), startX = from, endX = to),
                                Offset(from, r.top + cross * 0.15f), Size(to - from, cross * 0.7f), CornerRadius(cross * 0.35f),
                            )
                        }
                    }
                    // 选中的底色：竖向展开时往右伸长。
                    drawRoundRect(indicator, Offset(r.left, r.top), Size(r.width + grow, r.height), CornerRadius(cross / 2))
                },
        ) {
            val itemList: @Composable () -> Unit = {
                items.forEachIndexed { i, item ->
                    PillItem(
                        item, on = i == selected, vertical = vertical, maxLabel = maxLabel, reduced = reduced,
                        modifier = Modifier.onPlaced { bounds.items[i] = it; bounds.update(i) },
                    ) { onSelect(i) }
                }
            }
            if (vertical) {
                Column(Modifier.width(RailItem + PillPadding * 2).padding(vertical = PillPadding), horizontalAlignment = Alignment.CenterHorizontally) {
                    header?.let { Box(Modifier.size(RailItem), contentAlignment = Alignment.Center) { it() } }
                    Column(
                        // 按项目区等分，header 不算。
                        Modifier.pillDrag(drag, items.size, selected, onSelect, vertical = true).selectableGroup(),
                        verticalArrangement = Arrangement.spacedBy(RailGap),
                    ) { itemList() }
                }
            } else {
                Row(Modifier.height(Dimens.pillHeight).padding(PillPadding), verticalAlignment = Alignment.CenterVertically) {
                    header?.let { Box(Modifier.size(Dimens.pillItemHeight), contentAlignment = Alignment.Center) { it() } }
                    Row(
                        Modifier.wrapContentWidth().pillDrag(drag, items.size, selected, onSelect).selectableGroup(),
                        verticalAlignment = Alignment.CenterVertically,
                    ) { itemList() }
                }
            }
            // 竖向展开时的名称：在选中那一项的图标右边，跟着那一项上下走，后半段才变清楚。
            if (open > 0.01f) {
                Text(
                    label, style = labelStyle, color = c.foreground, maxLines = 1, softWrap = false,
                    modifier = Modifier
                        .offset {
                            val r = slider.rect(bounds.rects, vertical = true) ?: return@offset IntOffset.Zero
                            val x = r.left + ((RailItem + IconSize) / 2 + LabelGap).toPx()
                            val y = r.center.y - 10.dp.toPx()
                            IntOffset(x.roundToInt(), y.roundToInt())
                        }
                        .graphicsLayer { alpha = open * open }
                        .clearAndSetSemantics {},
                )
            }
        }
    }
}

/**
 * 竖向展开时接缝处的过渡（像素）：一段半径 [r] 的圆弧，一头和胶囊的右边（x = [w]）相切，一头和鼓出部分的上边或下边相切；
 * 刚开始展开、鼓出的还只是一段圆头时，和圆头相切。返回圆弧和接缝之间要补上的那一小块，接缝不在胶囊的直边上时返回 null。
 * [bt]、[bb] 是鼓出部分的上下边，[right] 是它的右边，[cap] 是它两头圆角的半径，[frameTop]、[frameBottom] 是胶囊的上下边。
 */
private fun bulgeFillet(w: Float, frameTop: Float, frameBottom: Float, bt: Float, bb: Float, right: Float, cap: Float, r: Float, upper: Boolean): Path? {
    // 鼓出部分靠这一边的圆角的圆心；圆角左边是直边，圆弧落在直边上时圆心在 x = w + r 的正上（下）方。
    val capX = right - cap
    val capY = if (upper) bt + cap else bb - cap
    val dx = capX - (w + r)
    val dy = if (dx >= 0) cap + r else {
        val d2 = (cap + r) * (cap + r) - dx * dx
        if (d2 <= 0f) return null
        kotlin.math.sqrt(d2)
    }
    val oy = if (upper) capY - dy else capY + dy
    // 圆弧和胶囊右边的切点要在胶囊的直边上（避开上下两头的圆角）。
    if (oy < frameTop + w / 2 || oy > frameBottom - w / 2) return null
    val ox = w + r
    // 和鼓出部分的切点：直边上就在圆心正下（上）方，圆角上就在两个圆心的连线上。
    val (tx, ty) = if (dx >= 0) ox to (if (upper) oy + r else oy - r) else {
        val len = cap + r
        (ox + dx / len * r) to (oy + (capY - oy) / len * r)
    }
    val end = Math.toDegrees(kotlin.math.atan2((ty - oy).toDouble(), (tx - ox).toDouble())).toFloat()
    var sweep = end - 180f
    if (sweep > 180f) sweep -= 360f
    if (sweep < -180f) sweep += 360f
    return Path().apply {
        moveTo(w, oy)
        arcTo(Rect(ox - r, oy - r, ox + r, oy + r), 180f, sweep, false)
        // 回到胶囊里面再闭合：多出的部分都落在胶囊和鼓出部分里面，合并后看不出来。
        lineTo(w - 1f, ty)
        lineTo(w - 1f, oy)
        close()
    }
}

@Composable
private fun PillItem(item: PillNavItem, on: Boolean, vertical: Boolean, maxLabel: Dp, reduced: Boolean, modifier: Modifier = Modifier, onClick: () -> Unit) {
    val c = NagiTheme.colors
    val interaction = remember { MutableInteractionSource() }
    val pressed by interaction.collectIsPressedAsState()
    // 选中的进度：0 没选中，1 选中。横向时也是名称展开的进度；所有项用同一条弹簧，收起和展开的宽度正好抵消。
    val p by animateFloatAsState(if (on) 1f else 0f, if (reduced) snap() else Motion.itemSpring(), label = "expand")
    val iconColor = lerpColor(c.mutedForeground, c.foreground, p.coerceIn(0f, 1f))
    val size = if (vertical) Modifier.size(RailItem) else Modifier.width(ItemPadding * 2 + IconSize + (IconLabelGap + maxLabel) * p).height(Dimens.pillItemHeight)
    Box(
        modifier = modifier
            .then(size)
            .clip(CircleShape)
            .clipToBounds()
            .pressable(true, Role.Tab, null, interaction, pressed, onClick)
            .clearAndSetSemantics {
                contentDescription = item.label
                role = Role.Tab
                selected = on
            },
        contentAlignment = if (vertical) Alignment.Center else Alignment.CenterStart,
    ) {
        if (vertical) {
            PillIcon(item, iconColor)
        } else {
            // 名称比最长的短时在选中的一项里居中：左边多留一半的差。
            val measurer = rememberTextMeasurer()
            val labelStyle = NagiTheme.type.bodyMedium
            val density = LocalDensity.current
            val labelWidth = remember(item.label, labelStyle) { with(density) { measurer.measure(item.label, labelStyle, maxLines = 1).size.width.toDp() } }
            val start = ItemPadding + (maxLabel - labelWidth) / 2 * p
            Row(
                modifier = Modifier.wrapContentWidth(Alignment.Start, unbounded = true).padding(start = start),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                PillIcon(item, iconColor)
                Spacer(Modifier.width(IconLabelGap))
                // 名称在展开的后半段才变清楚，收起时先变淡（非线性）。
                Text(
                    item.label, style = labelStyle, color = c.foreground, maxLines = 1, softWrap = false,
                    modifier = Modifier.graphicsLayer { alpha = (p * p).coerceIn(0f, 1f) },
                )
            }
        }
    }
}

@Composable
private fun PillIcon(item: PillNavItem, tint: Color) {
    val c = NagiTheme.colors
    Box {
        Icon(item.icon, contentDescription = null, tint = tint, modifier = Modifier.size(IconSize))
        if (item.badge) {
            // 7 的红点，外面一圈 2 的胶囊底色描边，和图标分开。
            Box(
                Modifier
                    .align(Alignment.TopEnd)
                    .offset(x = 4.dp, y = (-3).dp)
                    .size(11.dp)
                    .clip(CircleShape)
                    .background(pillBackground(c, LocalNagiDark.current))
                    .padding(2.dp)
                    .clip(CircleShape)
                    .background(c.destructive),
            )
        }
    }
}

private fun lerpColor(a: Color, b: Color, t: Float) = Color(
    red = a.red + (b.red - a.red) * t,
    green = a.green + (b.green - a.green) * t,
    blue = a.blue + (b.blue - a.blue) * t,
    alpha = a.alpha + (b.alpha - a.alpha) * t,
)
