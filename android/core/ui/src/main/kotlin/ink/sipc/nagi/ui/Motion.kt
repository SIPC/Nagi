package ink.sipc.nagi.ui

import androidx.compose.animation.AnimatedContent
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
 * 「移除动画」时只淡入淡出。
 */
@Composable
fun Roll(text: String, style: TextStyle, color: Color, modifier: Modifier = Modifier) {
    val reduced = reducedMotion()
    // 上一次显示的值：用来判断变大还是变小。
    val last = remember { arrayOf(text) }
    val up = text == last[0] || (text.numberOrNull() ?: 0.0) >= (last[0].numberOrNull() ?: 0.0)
    SideEffect { last[0] = text }
    // 每一位是单独的 Text，无障碍里合成一个节点读整段文字：位数多时节点少很多，开着无障碍服务（例如系统截屏）时每帧不必重算这么多。
    Row(
        (if (reduced) modifier else modifier.animateContentSize(tween(Motion.ROLL, easing = Motion.Enter)))
            .clearAndSetSemantics { this.text = AnnotatedString(text) },
    ) {
        val n = text.length
        text.forEachIndexed { i, ch ->
            // 按从右数第几位做 key：位数变了，个位还是个位。
            val fromRight = n - 1 - i
            key(fromRight) {
                AnimatedContent(
                    targetState = ch,
                    transitionSpec = {
                        val d = Motion.ROLL_STAGGER * fromRight
                        if (reduced) {
                            fadeIn(tween(Motion.ROLL, d)) togetherWith fadeOut(tween(100, d))
                        } else {
                            (slideInVertically(tween(Motion.ROLL, d, Motion.Enter)) { h -> if (up) h else -h } +
                                fadeIn(tween(Motion.ROLL, d, Motion.Enter))) togetherWith
                                (slideOutVertically(tween(Motion.ROLL, d, Motion.Enter)) { h -> if (up) -h else h } +
                                    fadeOut(tween(Motion.ROLL / 2, d, Motion.Exit))) using
                                SizeTransform(clip = true) { _, _ -> tween(Motion.ROLL, d, Motion.Enter) }
                        }
                    },
                    label = "roll",
                ) { c -> Text(c.toString(), style = style, color = color, maxLines = 1) }
            }
        }
    }
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
