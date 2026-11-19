package ink.sipc.nagi.ui

import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.AnimatedContentTransitionScope
import androidx.compose.animation.AnimatedVisibilityScope
import androidx.compose.animation.ContentTransform
import androidx.compose.animation.EnterExitState
import androidx.compose.animation.ExperimentalAnimationApi
import androidx.compose.animation.core.animateFloat
import androidx.compose.foundation.layout.padding
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.draw.drawWithContent
import androidx.compose.ui.graphics.BlendMode
import androidx.compose.ui.graphics.BlurEffect
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.CompositingStrategy
import androidx.compose.ui.graphics.TileMode
import androidx.compose.ui.graphics.drawscope.clipRect
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.layout.layout
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.offset
import kotlin.math.roundToInt
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.animation.SizeTransform
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.animateContentSize
import androidx.compose.animation.core.Animatable
import androidx.compose.animation.core.CubicBezierEasing
import androidx.compose.animation.core.snap
import androidx.compose.animation.core.spring
import androidx.compose.animation.core.tween
import androidx.compose.animation.expandVertically
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.shrinkVertically
import androidx.compose.animation.slideInVertically
import androidx.compose.animation.slideOutVertically
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.layout.Row
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.text
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.State
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Rect
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.util.lerp
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch

// 动效，和网页同一套规则（docs/ui.md 的「动画」）：有用途、简短、随时能打断；系统开了「移除动画」时
// 位移和缩放归零，只剩淡入淡出，指示器直接到位。

object Motion {
    /** 内容滑入。 */
    val ContentIn = CubicBezierEasing(0.25f, 0.46f, 0.3f, 1f)
    /** 其他进入。 */
    val Enter = CubicBezierEasing(0.23f, 1f, 0.32f, 1f)
    /** 退出，比进入快。 */
    val Exit = CubicBezierEasing(0.4f, 0f, 1f, 1f)

    const val FADE_IN = 160          // 进下一级、第一次显示、骨架换成内容、提示出现
    const val HEIGHT = 360           // 高度变化（AutoHeight、Collapse）
    const val ROLL = 260             // 跳字
    const val ROLL_STAGGER = 16      // 跳字每一位错开
    const val INDICATOR_LEAD = 200   // 选中指示器：朝移动方向的边
    const val INDICATOR_TRAIL = 340  // 选中指示器：另一条边
    const val BAR = 220              // 占用条的长度变化
    const val DOT_COLOR = 300        // 状态点的颜色变化
    const val SHEET_IN = 220         // 底部面板
    const val SHEET_OUT = 160
    const val THEME = 300            // 换浅色、深色时颜色过渡
    const val ROOT_IN = 240          // 添加、解锁后进入首页这类整页的切换
    const val ROOT_OUT = 120

    // 选中指示器、胶囊里每一项的宽度用弹簧：非线性，到位时带一点回弹。朝移动方向的边快一些，另一条边慢一些跟上，
    // 中途略微拉长；网页的 200ms / 340ms 大致对应这两组参数的到位时间。
    fun <T> leadSpring() = spring<T>(dampingRatio = 0.72f, stiffness = 700f)
    fun <T> trailSpring() = spring<T>(dampingRatio = 0.72f, stiffness = 380f)
    /** 胶囊里每一项的宽度、名称的展开：和慢的那条边同一条曲线，宽度变化和底色移动是同一个动作。 */
    fun <T> itemSpring() = spring<T>(dampingRatio = 0.8f, stiffness = 380f)
    /** 拖尾：比慢的那条边再慢一点、不回弹，跟在指示器后面，到位时缩回。 */
    fun <T> ghostSpring() = spring<T>(dampingRatio = 1f, stiffness = 220f)
}

/** 系统是否开了「移除动画」。 */
@Composable
fun isReducedMotion(): Boolean = reducedMotion()

/**
 * 高度变化（AutoHeight）：数据加载完、列表增减引起的高度变化，过渡到新高度，下面的内容跟着移动。
 * 每个组件各自过渡，不过渡整页。
 */
@Composable
fun Modifier.autoHeight(): Modifier =
    if (reducedMotion()) this else animateContentSize(tween(Motion.HEIGHT, easing = Motion.ContentIn))

/**
 * 切换后多出、少掉的一块（Collapse）：高度和透明度一起过渡，下面的内容跟着移动；
 * 收起时内容留到过渡结束才移除。
 */
@Composable
fun Collapse(visible: Boolean, modifier: Modifier = Modifier, content: @Composable ColumnScope.() -> Unit) {
    val reduced = reducedMotion()
    AnimatedVisibility(
        visible = visible,
        modifier = modifier,
        enter = if (reduced) fadeIn(tween(Motion.FADE_IN)) else
            expandVertically(tween(Motion.HEIGHT, easing = Motion.ContentIn)) + fadeIn(tween(Motion.HEIGHT, easing = Motion.Enter)),
        exit = if (reduced) fadeOut(tween(100)) else
            shrinkVertically(tween(Motion.HEIGHT, easing = Motion.ContentIn)) + fadeOut(tween(Motion.HEIGHT, easing = Motion.Exit)),
    ) {
        Column(content = content)
    }
}

/** 状态点的颜色变化（300ms）。 */
@Composable
fun animatedDotColor(target: Color): State<Color> = animateColorAsState(target, tween(Motion.DOT_COLOR), label = "dot")

/**
 * 跳字（Roll）：会变的数据（监控数字、占用、速率、数量）只有变了的那几位上下滚动，变大从下面滚上来，变小从上面滚下来，
 * 从右往左依次错开 16ms；每一位的宽度也过渡，位数变化时后面的内容平滑让位。第一次显示不滚。
 * 滚动的文字不在行的边缘硬切，上下渐隐、略微模糊（[rollTransform]、[rollFadeEdges]、[rollBlur]）。
 * 「移除动画」时只淡入淡出。
 */
@Composable
fun Roll(text: String, style: TextStyle, color: Color, modifier: Modifier = Modifier) {
    val reduced = reducedMotion()
    val density = LocalDensity.current
    // 上一次显示的值：用来判断变大还是变小。
    val last = remember { arrayOf(text) }
    val up = text == last[0] || (text.numberOrNull() ?: 0.0) >= (last[0].numberOrNull() ?: 0.0)
    SideEffect { last[0] = text }
    val band = with(density) { (if (style.lineHeight.isSp) style.lineHeight else style.fontSize).toDp() } * ROLL_FADE
    // 每一位是单独的 Text，无障碍里合成一个节点读整段文字：位数多时节点少很多，开着无障碍服务（例如系统截屏）时每帧不必重算这么多。
    Row(
        modifier
            .rollWidth(!reduced)
            .rollFadeEdges(band)
            .clearAndSetSemantics { this.text = AnnotatedString(text) },
    ) {
        val n = text.length
        text.forEachIndexed { i, ch ->
            // 按从右数第几位做 key：位数变了，个位还是个位。
            val fromRight = n - 1 - i
            val d = Motion.ROLL_STAGGER * fromRight
            key(fromRight) {
                AnimatedContent(
                    targetState = ch,
                    transitionSpec = { rollTransform(up, reduced, d) },
                    label = "roll",
                ) { c -> Text(c.toString(), style = style, color = color, maxLines = 1, modifier = rollBlur(reduced, d)) }
            }
        }
    }
}

/** 滚动文字上下渐隐的那一段，是行高的几倍。 */
internal const val ROLL_FADE = 0.4f

/**
 * 上下滚动的文字切换（跳字、竖向胶囊的名称共用）：新的从下面（[up]）或上面滑进来，旧的往反方向滑出去，同时淡入淡出。
 * 不裁切，滚出去的部分由外面的 [rollFadeEdges] 渐隐。[delay] 是错开的毫秒数。
 */
internal fun <S> AnimatedContentTransitionScope<S>.rollTransform(up: Boolean, reduced: Boolean, delay: Int = 0): ContentTransform =
    if (reduced) {
        fadeIn(tween(Motion.ROLL, delay)) togetherWith fadeOut(tween(100, delay)) using SizeTransform(clip = false)
    } else {
        (slideInVertically(tween(Motion.ROLL, delay, Motion.Enter)) { h -> if (up) h else -h } +
            fadeIn(tween(Motion.ROLL, delay, Motion.Enter))) togetherWith
            (slideOutVertically(tween(Motion.ROLL, delay, Motion.Enter)) { h -> if (up) -h else h } +
                fadeOut(tween(Motion.ROLL / 2, delay, Motion.Exit))) using
            SizeTransform(clip = false) { _, _ -> tween(Motion.ROLL, delay, Motion.Enter) }
    }

/**
 * 滚动文字的上下边缘：在原位置上下各多画 [band]（不占布局，不影响周围的排版），这一段从不透明渐变到透明，
 * 滚出去的文字逐渐消失，不会被一刀切断。原来那一行里的文字不受影响。
 */
internal fun Modifier.rollFadeEdges(band: Dp): Modifier = this
    .layout { m, c ->
        val pad = band.roundToPx()
        val p = m.measure(c.offset(vertical = 2 * pad))
        layout(p.width, (p.height - 2 * pad).coerceAtLeast(0)) { p.place(0, -pad) }
    }
    .graphicsLayer { compositingStrategy = CompositingStrategy.Offscreen }
    .drawWithContent {
        drawContent()
        val f = (band.toPx() / size.height).coerceIn(0f, 0.5f)
        drawRect(
            Brush.verticalGradient(0f to Color.Transparent, f to Color.Black, 1f - f to Color.Black, 1f to Color.Transparent),
            blendMode = BlendMode.DstIn,
        )
    }
    .padding(vertical = band)

/** 滚动时最多模糊多少。 */
private val ROLL_BLUR = 2.dp

/**
 * 滚动时的轻微模糊：滑进来的从模糊变清楚，滑出去的变模糊（最多 [ROLL_BLUR]），和滑动同一条曲线。
 * 只在 Android 12 以上有（更早的系统没有模糊，只渐隐）；动画时只改图层，不重组。
 */
@OptIn(ExperimentalAnimationApi::class)
@Composable
internal fun AnimatedVisibilityScope.rollBlur(reduced: Boolean, delay: Int = 0): Modifier {
    if (reduced || android.os.Build.VERSION.SDK_INT < android.os.Build.VERSION_CODES.S) return Modifier
    val amount = transition.animateFloat({ tween(Motion.ROLL, delay, Motion.Enter) }, label = "blur") { if (it == EnterExitState.Visible) 0f else 1f }
    return Modifier.graphicsLayer {
        val r = ROLL_BLUR.toPx() * amount.value
        renderEffect = if (r > 0.1f) BlurEffect(r, r, TileMode.Decal) else null
    }
}

/**
 * 宽度过渡（跳字的位数变化时）：和 animateContentSize 一样，但只在左右裁切，上下不裁，滚动的文字可以滚出这一行再渐隐。
 */
@Composable
private fun Modifier.rollWidth(enabled: Boolean): Modifier {
    if (!enabled) return this
    val width = remember { Animatable(-1f) }
    val scope = rememberCoroutineScope()
    return this
        .layout { m, c ->
            val p = m.measure(c.copy(minWidth = 0))
            val target = p.width.toFloat()
            if (width.value < 0f) scope.launch { width.snapTo(target) }
            else if (width.targetValue != target) scope.launch { width.animateTo(target, tween(Motion.ROLL, easing = Motion.Enter)) }
            val w = (if (width.value < 0f) target else width.value).roundToInt().coerceIn(c.minWidth, c.maxWidth)
            layout(w, p.height) { p.place(0, 0) }
        }
        .drawWithContent { clipRect(0f, -size.height, size.width, size.height * 2) { this@drawWithContent.drawContent() } }
}

private fun String.numberOrNull(): Double? = filter { it.isDigit() || it == '.' || it == '-' }.toDoubleOrNull()

/**
 * 选中指示器（胶囊导航的底色、标签和选择项的下划线）：从上一项滑过去，朝移动方向的边先到（leadSpring），
 * 另一条边跟上（trailSpring），中途略微拉长，到位时带一点回弹。两项的位置可以在动画中变化（例如胶囊里名称在展开），
 * 指示器的两条边一直按各自的进度落在两项实时的位置之间。
 */
class SlidingIndicator internal constructor(
    private val lead: Animatable<Float, *>,
    private val trail: Animatable<Float, *>,
    private val ghost: Animatable<Float, *>,
    private val toState: State<Int>,
) {
    /** 这次滑动的起点：开始滑时画在哪里。中途换了目标，从当时的位置接着滑，不跳。 */
    internal var start: Rect? = null
    /** 上一帧画出来的位置。 */
    internal var drawn: Rect? = null

    /**
     * 按目标项当前的位置算出指示器的位置；bounds 里还没有目标项时返回 null。
     * [vertical] 为 true 时沿上下滑（竖向的胶囊），否则沿左右。
     */
    fun rect(bounds: Map<Int, Rect>, vertical: Boolean = false): Rect? {
        val b = bounds[toState.value] ?: return null
        val a = start ?: b
        // 朝移动方向的边用快的进度，另一条边用慢的。
        val r = if (vertical) {
            val movingDown = b.center.y >= a.center.y
            val pTop = if (movingDown) trail.value else lead.value
            val pBottom = if (movingDown) lead.value else trail.value
            Rect(b.left, lerp(a.top, b.top, pTop), b.right, lerp(a.bottom, b.bottom, pBottom))
        } else {
            val movingRight = b.center.x >= a.center.x
            val pLeft = if (movingRight) trail.value else lead.value
            val pRight = if (movingRight) lead.value else trail.value
            Rect(lerp(a.left, b.left, pLeft), b.top, lerp(a.right, b.right, pRight), b.bottom)
        }
        drawn = r
        return r
    }

    /**
     * 拖尾：滑动时跟在指示器后面的一小段，由比两条边都慢的进度（ghostSpring）算出，最长 [maxLength]，
     * 到位时缩回没有。返回拖尾沿滑动方向的两端（只在指示器后面那一侧）；没在滑动时返回 null。[r] 是这一帧的 [rect]。
     */
    fun tail(bounds: Map<Int, Rect>, r: Rect, maxLength: Float, vertical: Boolean = false): Pair<Float, Float>? {
        val b = bounds[toState.value] ?: return null
        val a = start ?: return null
        if (ghost.value >= 1f) return null
        if (vertical) {
            val movingDown = b.center.y >= a.center.y
            return if (movingDown) {
                val end = lerp(a.top, b.top, ghost.value)
                val from = maxOf(end, r.top - maxLength)
                if (from >= r.top - 0.5f) null else from to r.top + r.width / 2
            } else {
                val end = lerp(a.bottom, b.bottom, ghost.value)
                val to = minOf(end, r.bottom + maxLength)
                if (to <= r.bottom + 0.5f) null else r.bottom - r.width / 2 to to
            }
        }
        val movingRight = b.center.x >= a.center.x
        return if (movingRight) {
            val end = lerp(a.left, b.left, ghost.value)
            val from = maxOf(end, r.left - maxLength)
            if (from >= r.left - 0.5f) null else from to r.left + r.height / 2
        } else {
            val end = lerp(a.right, b.right, ghost.value)
            val to = minOf(end, r.right + maxLength)
            if (to <= r.right + 0.5f) null else r.right - r.height / 2 to to
        }
    }
}

/** [selected] 变化时让指示器滑过去。 */
@Composable
fun rememberSlidingIndicator(selected: Int): SlidingIndicator {
    val reduced = reducedMotion()
    // lead 是快的那条边的进度，trail 是慢的；两者都从 0 到 1，弹簧到位时略微越过 1 再回来。
    val lead = remember { Animatable(1f) }
    val trail = remember { Animatable(1f) }
    val ghost = remember { Animatable(1f) }
    val to = remember { mutableIntStateOf(selected) }
    val indicator = remember { SlidingIndicator(lead, trail, ghost, to) }
    LaunchedEffect(selected) {
        if (selected == to.intValue) return@LaunchedEffect
        indicator.start = indicator.drawn
        to.intValue = selected
        lead.snapTo(0f)
        trail.snapTo(0f)
        ghost.snapTo(0f)
        if (reduced) {
            lead.snapTo(1f)
            trail.snapTo(1f)
            ghost.snapTo(1f)
        } else coroutineScope {
            launch { lead.animateTo(1f, Motion.leadSpring()) }
            launch { trail.animateTo(1f, Motion.trailSpring()) }
            launch { ghost.animateTo(1f, Motion.ghostSpring()) }
        }
    }
    return indicator
}

/** 占用条长度变化的时长；「移除动画」时直接到位。 */
@Composable
fun barSpec() = if (reducedMotion()) snap<Float>() else tween<Float>(Motion.BAR, easing = Motion.Enter)
