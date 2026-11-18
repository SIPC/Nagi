package ink.sipc.nagi.ui

import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.snap
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.collectIsPressedAsState
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
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
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.draw.dropShadow
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Rect
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.graphics.shadow.Shadow
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.onPlaced
import androidx.compose.ui.layout.positionInParent
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.DpOffset
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.toSize

data class PillNavItem(val icon: ImageVector, val label: String, val badge: Boolean = false)

// 胶囊里的尺寸（docs/android.md 导航）：胶囊高 48、内边距 4，每一项高 40、左右各 12，图标 20，图标和名称之间 4。
private val PillPadding = 4.dp
private val ItemPadding = 13.dp
private val IconSize = 22.dp
private val IconLabelGap = 4.dp

/** 底色回弹越出两端时，外框撑开的距离是越出距离的几倍，以及最多撑开多少。 */
private const val FRAME_FOLLOW = 1f
private val FRAME_MAX = 6.dp

/** 选中底色的拖尾最长多少。 */
private val TAIL_MAX = 20.dp

// PillNav：底部悬浮的胶囊导航。按内容宽度在底部居中，距手势条 8。
// 每一项占固定的位置：没选中的宽 48（只有图标），选中的一项按最长的名称留宽度，名称在里面居中。
// 切换时旧的一项收起、新的一项展开，用同一条弹簧，一减一加正好抵消，所以胶囊的总宽度不变，不会跳。
// 选中的底色从旧的一项滑到新的一项（rememberSlidingIndicator：朝移动方向的边快、另一条边慢，带一点回弹）。
// 长按胶囊后左右拖动：手指经过哪一项就切到哪一项（名称展开、页面切换，和点击一样），每切一次轻振一下；
// 按住时胶囊轻微放大，松手回弹。手指下是哪一项按胶囊等分来算：各项宽度在变，但总宽度不变，等分是稳定的，不会来回跳。
// 系统开了「移除动画」时都直接到位。badge 是图标右上角的红点（有严重告警时）。
@Composable
fun PillNav(
    items: List<PillNavItem>,
    selected: Int,
    onSelect: (Int) -> Unit,
    modifier: Modifier = Modifier,
) {
    val c = NagiTheme.colors
    val dark = LocalNagiDark.current
    val reduced = reducedMotion()
    val density = LocalDensity.current

    // 最长的名称宽度：选中的一项按它留位置，换到哪一项胶囊都一样宽。
    val measurer = rememberTextMeasurer()
    val labelStyle = NagiTheme.type.bodyMedium
    val maxLabel = remember(items, labelStyle) {
        with(density) { items.maxOf { measurer.measure(it.label, labelStyle, maxLines = 1).size.width }.toDp() }
    }

    val drag = rememberPillDrag()
    val scale = drag.scale()
    val bounds = remember { mutableStateMapOf<Int, Rect>() }
    val slider = rememberSlidingIndicator(selected)
    val indicator = c.muted
    val card = c.card
    val border = c.border
    // 长按拖动见 PillGestures.kt（和宽屏左侧的 PillRail 共用）。
    PillLongPress {
    Box(
        modifier = modifier.fillMaxWidth().navigationBarsPadding().padding(bottom = 12.dp),
        contentAlignment = Alignment.BottomCenter,
    ) {
        Row(
            modifier = Modifier
                .wrapContentWidth()
                .height(Dimens.pillHeight)
                .graphicsLayer { scaleX = scale; scaleY = scale }
                .dropShadow(CircleShape, Shadow(radius = 16.dp, color = Color.Black, offset = DpOffset(0.dp, 4.dp), alpha = if (dark) 0.5f else 0.08f))
                .padding(PillPadding)
                .drawBehind {
                    // 外框和底色一起画：切到最左、最右一项（点击或拖动）时，底色回弹越出内容区，外框在那一侧跟着撑开，
                    // 回弹结束一起收回。弹簧的过冲只有 1–2dp，看不出来，所以外框按越出距离的 FRAME_FOLLOW 倍撑开，最多 FRAME_MAX。
                    // 中间几项的回弹在框内，外框不动。
                    val r = slider.rect(bounds)
                    val pad = PillPadding.toPx()
                    val maxOver = FRAME_MAX.toPx()
                    val overLeft = if (r != null) (maxOf(0f, -r.left) * FRAME_FOLLOW).coerceAtMost(maxOver) else 0f
                    val overRight = if (r != null) (maxOf(0f, r.right - size.width) * FRAME_FOLLOW).coerceAtMost(maxOver) else 0f
                    val frame = Rect(-pad - overLeft, -pad, size.width + pad + overRight, size.height + pad)
                    val radius = CornerRadius(frame.height / 2)
                    drawRoundRect(card, frame.topLeft, frame.size, radius)
                    val stroke = 1.dp.toPx()
                    drawRoundRect(
                        border, Offset(frame.left + stroke / 2, frame.top + stroke / 2), Size(frame.width - stroke, frame.height - stroke),
                        CornerRadius((frame.height - stroke) / 2), style = Stroke(stroke),
                    )
                    if (r != null) {
                        // 拖尾：指示器后面一小段（最长 TAIL_MAX），从指示器的颜色渐变到透明，滑到位时缩回。
                        slider.tail(bounds, r, TAIL_MAX.toPx())?.let { (from, to) ->
                            val leftFade = from < r.left
                            drawRoundRect(
                                Brush.horizontalGradient(
                                    if (leftFade) listOf(indicator.copy(alpha = 0f), indicator) else listOf(indicator, indicator.copy(alpha = 0f)),
                                    startX = from, endX = to,
                                ),
                                Offset(from, r.top + r.height * 0.15f), Size(to - from, r.height * 0.7f), CornerRadius(r.height * 0.35f),
                            )
                        }
                        drawRoundRect(indicator, Offset(r.left, r.top), Size(r.width, r.height), CornerRadius(r.height / 2))
                    }
                }
                .pillDrag(drag, items.size, selected, onSelect)
                .selectableGroup(),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            items.forEachIndexed { i, item ->
                PillItem(
                    item,
                    on = i == selected,
                    maxLabel = maxLabel,
                    reduced = reduced,
                    modifier = Modifier.onPlaced { bounds[i] = Rect(it.positionInParent(), it.size.toSize()) },
                ) { onSelect(i) }
            }
        }
    }
    }
}

@Composable
private fun PillItem(item: PillNavItem, on: Boolean, maxLabel: Dp, reduced: Boolean, modifier: Modifier = Modifier, onClick: () -> Unit) {
    val c = NagiTheme.colors
    val interaction = remember { MutableInteractionSource() }
    val pressed by interaction.collectIsPressedAsState()
    // 展开的进度：0 只有图标，1 图标加名称。所有项用同一条弹簧，所以收起和展开的宽度正好抵消。
    val p by animateFloatAsState(if (on) 1f else 0f, if (reduced) snap() else Motion.itemSpring(), label = "expand")
    val extra = IconLabelGap + maxLabel
    val width = ItemPadding * 2 + IconSize + extra * p
    // 名称比最长的短时在选中的一项里居中：左边多留一半的差。
    val measurer = rememberTextMeasurer()
    val labelStyle = NagiTheme.type.bodyMedium
    val density = LocalDensity.current
    val labelWidth = remember(item.label, labelStyle) { with(density) { measurer.measure(item.label, labelStyle, maxLines = 1).size.width.toDp() } }
    val start = ItemPadding + (maxLabel - labelWidth) / 2 * p
    val iconColor = lerpColor(c.mutedForeground, c.foreground, p.coerceIn(0f, 1f))
    Box(
        modifier = modifier
            .width(width)
            .height(Dimens.pillItemHeight)
            .clip(CircleShape)
            .clipToBounds()
            .pressable(true, Role.Tab, null, interaction, pressed, onClick)
            .clearAndSetSemantics {
                contentDescription = item.label
                role = Role.Tab
                selected = on
            },
        contentAlignment = Alignment.CenterStart,
    ) {
        Row(
            modifier = Modifier.wrapContentWidth(Alignment.Start, unbounded = true).padding(start = start),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Box {
                Icon(item.icon, contentDescription = null, tint = iconColor, modifier = Modifier.size(IconSize))
                if (item.badge) {
                    // 7 的红点，外面一圈 2 的 card 色描边，和图标分开
                    Box(
                        Modifier
                            .align(Alignment.TopEnd)
                            .offset(x = 4.dp, y = (-3).dp)
                            .size(11.dp)
                            .clip(CircleShape)
                            .background(c.card)
                            .padding(2.dp)
                            .clip(CircleShape)
                            .background(c.destructive),
                    )
                }
            }
            Spacer(Modifier.width(IconLabelGap))
            // 名称在展开的后半段才变清楚，收起时先变淡（非线性）。
            Text(
                item.label,
                style = NagiTheme.type.bodyMedium,
                color = c.foreground,
                maxLines = 1,
                softWrap = false,
                modifier = Modifier.graphicsLayer { alpha = (p * p).coerceIn(0f, 1f) },
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
