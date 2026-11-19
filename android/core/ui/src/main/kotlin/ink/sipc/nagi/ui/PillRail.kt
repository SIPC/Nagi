package ink.sipc.nagi.ui

import androidx.compose.animation.core.Animatable
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.snap
import androidx.compose.foundation.background
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.collectIsPressedAsState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.RoundRect
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.asAndroidPath
import androidx.compose.ui.graphics.nativeCanvas
import androidx.compose.ui.graphics.PathOperation
import androidx.compose.ui.graphics.TransformOrigin
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.drawIntoCanvas
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.dp
import kotlin.math.roundToInt

// PillRail：宽屏（600 dp 以上）时代替底部的胶囊导航，竖在左栏的左下角（docs/android.md 宽屏）：最上面是 logo（不可点），
// 下面是四个图标。样式和底部的胶囊一样：card 底、1px 边框、浅阴影、muted 的选中底色，选中底色从上一项滑过去。
// 长按后上下拖动切换（和底部的胶囊共用，PillGestures.kt）。拖动时选中的那一项横着展开：底色往右伸长、图标右边淡入名称，
// 胶囊的外框在那一行跟着鼓出来，和胶囊连成一个形状（同一个底色、同一圈边框，接缝处用圆弧过渡），松手后用弹簧收回。
// 按住时整个胶囊以中心放大。
// 展开的部分盖在右边的内容上面，左栏要放在内容的上面一层（zIndex）。

private val RailWidth = 64.dp
private val RailItem = 56.dp
private val RailGap = 4.dp
private val RailPad = 4.dp
private val IconSize = 22.dp
private val LabelGap = 9.dp
// 名称右边到底色边缘的距离：比图标左边的留白窄 3，名称在图标右边的空当里看起来居中。
private val LabelEnd = 14.dp

@Composable
fun PillRail(items: List<PillNavItem>, selected: Int, onSelect: (Int) -> Unit, modifier: Modifier = Modifier) {
    val c = NagiTheme.colors
    val dark = LocalNagiDark.current
    val reduced = reducedMotion()
    val density = LocalDensity.current
    val step = with(density) { (RailItem + RailGap).toPx() }
    // 选中的那一项的位置（第几项，可以是小数）：弹簧过去，到位时带一点回弹。
    val pos = remember { Animatable(selected.toFloat()) }
    LaunchedEffect(selected) {
        if (reduced) pos.snapTo(selected.toFloat()) else pos.animateTo(selected.toFloat(), Motion.trailSpring())
    }
    val drag = rememberPillDrag()
    val scale = drag.scale()
    // 展开的进度：0 只有图标，1 底色伸长、名称完整显示。和胶囊里每一项的展开同一条弹簧。
    val open by animateFloatAsState(if (drag.dragging && !reduced) 1f else 0f, if (reduced) snap() else Motion.itemSpring(), label = "open")
    // 展开后多出来的宽度：名称从图标右边 LabelGap 处开始，名称右边留 LabelEnd，
    // 减去原来那一项已有的宽度。按当前选中的名称算，名称变了宽度跟着过渡。
    val measurer = rememberTextMeasurer()
    val labelStyle = NagiTheme.type.bodyMedium
    val label = items.getOrNull(selected)?.label.orEmpty()
    val labelWidth = with(density) { measurer.measure(label, labelStyle, maxLines = 1).size.width.toDp() }
    val iconInset = (RailItem - IconSize) / 2
    val extraTarget = iconInset + IconSize + LabelGap + labelWidth + LabelEnd - RailItem
    val extra by androidx.compose.animation.core.animateDpAsState(extraTarget, if (reduced) snap() else Motion.itemSpring(), label = "extra")
    val shadowColor = if (dark) 0x80000000.toInt() else 0x14000000

    PillLongPress {
        Box(
            modifier
                .graphicsLayer { scaleX = scale; scaleY = scale; transformOrigin = TransformOrigin.Center }
                .drawBehind {
                    val w = RailWidth.toPx()
                    val rail = RoundRect(0f, 0f, w, size.height, CornerRadius(w / 2))
                    val rowTop = (RailPad + RailItem).toPx() + pos.value * step
                    val grow = extra.toPx() * open
                    // 外框：胶囊本身，加上展开那一行往右鼓出的部分（比那一项上下各大 4），两者合成一个形状；
                    // 鼓出的部分和胶囊的接缝处用圆弧过渡，不留尖角。
                    val bt = rowTop - RailPad.toPx()
                    val bb = rowTop + (RailItem + RailPad).toPx()
                    val bulge = RoundRect(0f, bt, w + grow, bb, CornerRadius((bb - bt) / 2))
                    val frame = Path().apply { addRoundRect(rail) }
                    if (grow > 0.5f) {
                        frame.op(frame, Path().apply { addRoundRect(bulge) }, PathOperation.Union)
                        for (top in listOf(true, false)) {
                            railFillet(w, size.height, bt, bb, w + grow, BulgeFillet.toPx(), top)?.let { frame.op(frame, it, PathOperation.Union) }
                        }
                    }
                    drawIntoCanvas { canvas ->
                        val paint = android.graphics.Paint().apply {
                            isAntiAlias = true
                            color = c.card.toArgb()
                            setShadowLayer(16.dp.toPx(), 0f, 4.dp.toPx(), shadowColor)
                        }
                        canvas.nativeCanvas.drawPath(frame.asAndroidPath(), paint)
                    }
                    drawPath(frame, c.border, style = Stroke(1.dp.toPx()))
                    // 选中的底色：在那一项上，展开时往右伸长。
                    val inset = ((RailWidth - RailItem) / 2).toPx()
                    val item = RailItem.toPx()
                    drawRoundRect(c.muted, Offset(inset, rowTop), Size(item + grow, item), CornerRadius(item / 2))
                },
        ) {
            Column(
                Modifier.width(RailWidth).padding(vertical = RailPad),
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                Box(Modifier.size(RailItem), contentAlignment = Alignment.Center) { NagiLogo(Modifier.size(28.dp)) }
                Column(
                    // 长按后上下拖动切换：按项目区等分，logo 不算。
                    Modifier.pillDrag(drag, items.size, selected, onSelect, vertical = true).selectableGroup(),
                    verticalArrangement = Arrangement.spacedBy(RailGap),
                ) {
                    items.forEachIndexed { i, item -> RailItem(item, i == selected) { onSelect(i) } }
                }
            }
            // 展开时的名称：在选中那一项的图标右边，跟着那一项上下走，后半段才变清楚（和胶囊里名称的淡入一样）。
            if (open > 0.01f) {
                Text(
                    label, style = labelStyle, color = c.foreground, maxLines = 1, softWrap = false,
                    modifier = Modifier
                        .offset {
                            val x = ((RailWidth - RailItem) / 2 + (RailItem + IconSize) / 2 + LabelGap).roundToPx()
                            val y = (RailPad + RailItem).roundToPx() + (pos.value * step).roundToInt() + ((RailItem - 20.dp) / 2).roundToPx()
                            IntOffset(x, y)
                        }
                        .graphicsLayer { alpha = open * open }
                        .clearAndSetSemantics {},
                )
            }
        }
    }
}

/** 鼓出的部分和胶囊接缝处的圆弧半径。 */
private val BulgeFillet = 14.dp

/**
 * 接缝处的过渡（像素）：一段半径 [r] 的圆弧，一头和胶囊的右边（x = [w]）相切，一头和鼓出部分的上边或下边相切；
 * 刚开始展开、鼓出的还只是一段圆头时，和圆头相切。返回圆弧和接缝之间要补上的那一小块，接缝不在胶囊的直边上时返回 null。
 * [bt]、[bb] 是鼓出部分的上下边，[right] 是它的右边，[height] 是胶囊的高度。
 */
private fun railFillet(w: Float, height: Float, bt: Float, bb: Float, right: Float, r: Float, top: Boolean): Path? {
    val radius = (bb - bt) / 2
    val cy = (bt + bb) / 2
    // 鼓出部分右边圆头的圆心；圆头左边是直边，圆弧落在直边上时圆心在 x = w + r 的正上（下）方。
    val capX = right - radius
    val dx = capX - (w + r)
    val dy = if (dx >= 0) radius + r else {
        val d2 = (radius + r) * (radius + r) - dx * dx
        if (d2 <= 0f) return null
        kotlin.math.sqrt(d2)
    }
    val oy = if (top) cy - dy else cy + dy
    // 圆弧和胶囊右边的切点要在胶囊的直边上（避开上下两头的圆角）。
    if (oy < w / 2 || oy > height - w / 2) return null
    val ox = w + r
    // 和鼓出部分的切点：直边上就在圆心正下（上）方，圆头上就在两个圆心的连线上。
    val (tx, ty) = if (dx >= 0) ox to (if (top) oy + r else oy - r) else {
        val len = radius + r
        (ox + dx / len * r) to (oy + (cy - oy) / len * r)
    }
    val end = Math.toDegrees(kotlin.math.atan2((ty - oy).toDouble(), (tx - ox).toDouble())).toFloat()
    var sweep = end - 180f
    if (sweep > 180f) sweep -= 360f
    if (sweep < -180f) sweep += 360f
    return Path().apply {
        moveTo(w, oy)
        arcTo(androidx.compose.ui.geometry.Rect(ox - r, oy - r, ox + r, oy + r), 180f, sweep, false)
        // 回到胶囊里面再闭合：多出的部分都落在胶囊和鼓出部分里面，合并后看不出来。
        lineTo(w - 1f, ty)
        lineTo(w - 1f, oy)
        close()
    }
}

@Composable
private fun RailItem(item: PillNavItem, on: Boolean, onClick: () -> Unit) {
    val c = NagiTheme.colors
    val interaction = remember { MutableInteractionSource() }
    val pressed by interaction.collectIsPressedAsState()
    Box(
        Modifier.size(RailItem).clip(RoundedCornerShape(RailItem / 2))
            .pressable(true, Role.Tab, null, interaction, pressed, onClick)
            .clearAndSetSemantics {
                contentDescription = item.label
                role = Role.Tab
                selected = on
            },
        contentAlignment = Alignment.Center,
    ) {
        Box {
            Icon(item.icon, contentDescription = null, tint = if (on) c.foreground else c.mutedForeground, modifier = Modifier.size(IconSize))
            if (item.badge) {
                Box(
                    Modifier.align(Alignment.TopEnd).offset(x = 4.dp, y = (-3).dp).size(11.dp).clip(RoundedCornerShape(6.dp))
                        .background(c.card).padding(2.dp).clip(RoundedCornerShape(4.dp)).background(c.destructive),
                )
            }
        }
    }
}
