package ink.sipc.nagi.ui

import androidx.compose.animation.ContentTransform
import androidx.compose.animation.EnterTransition
import androidx.compose.animation.ExitTransition
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.slideInVertically
import androidx.compose.animation.slideOutVertically
import androidx.compose.animation.togetherWith

/**
 * 页面切换（见 docs/android-ui.md#页面切换）。[reduced] 是系统开了「移除动画」，这时只淡入淡出。
 * - push / pop：进下一级时新页面从右边滑入，旧页面往左让 1/4 并淡出；返回反过来。用在 NavHost 的四个 transition 上。
 * - tab：主导航切换，跟导航同方向滑入 32 并淡入（透明度 120ms 先到，共 300ms），旧的同方向移开 16、100ms 淡出。
 */
object NagiTransitions {
    fun pushIn(reduced: Boolean): EnterTransition =
        if (reduced) fadeIn(tween(Motion.FADE_IN)) else slideInHorizontally(tween(300, easing = Motion.ContentIn)) { it } + fadeIn(tween(120))

    fun pushOut(reduced: Boolean): ExitTransition =
        if (reduced) fadeOut(tween(100)) else slideOutHorizontally(tween(300, easing = Motion.ContentIn)) { -it / 4 } + fadeOut(tween(300))

    fun popIn(reduced: Boolean): EnterTransition =
        if (reduced) fadeIn(tween(Motion.FADE_IN)) else slideInHorizontally(tween(300, easing = Motion.ContentIn)) { -it / 4 } + fadeIn(tween(300))

    fun popOut(reduced: Boolean): ExitTransition =
        if (reduced) fadeOut(tween(100)) else slideOutHorizontally(tween(300, easing = Motion.ContentIn)) { it } + fadeOut(tween(100, 200))

    /**
     * 主导航切换。[forward] 为 true 时选的是右边（宽屏时是下面）的标签；[inPx]、[outPx] 是 32dp、16dp 换成的像素。
     * [vertical]：宽屏的导航竖在左边，页面跟着上下滑（选下面的标签，新页面从下面进来，旧页面往上让开）。
     */
    fun tab(forward: Boolean, inPx: Int, outPx: Int, reduced: Boolean, vertical: Boolean = false): ContentTransform {
        if (reduced) return fadeIn(tween(Motion.FADE_IN)) togetherWith fadeOut(tween(100))
        val dir = if (forward) 1 else -1
        val enter = if (vertical) slideInVertically(tween(300, easing = Motion.ContentIn)) { dir * inPx } else slideInHorizontally(tween(300, easing = Motion.ContentIn)) { dir * inPx }
        val exit = if (vertical) slideOutVertically(tween(100, easing = Motion.Exit)) { -dir * outPx } else slideOutHorizontally(tween(100, easing = Motion.Exit)) { -dir * outPx }
        return (enter + fadeIn(tween(120, easing = Motion.Enter))) togetherWith (exit + fadeOut(tween(100, easing = Motion.Exit)))
    }
}
