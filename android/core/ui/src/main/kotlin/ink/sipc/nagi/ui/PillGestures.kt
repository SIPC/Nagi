package ink.sipc.nagi.ui

import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.snap
import androidx.compose.animation.core.spring
import androidx.compose.foundation.gestures.detectDragGesturesAfterLongPress
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Stable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.platform.LocalViewConfiguration
import androidx.compose.ui.platform.ViewConfiguration

// 胶囊导航 PillNav 的长按拖动（横向、竖向共用，见 docs/android.md 导航）：
// 按住 PILL_LONG_PRESS_MS 后拖动，手指经过哪一项就切到哪一项，每切一次轻振一下；按住时胶囊放大 4%，松手回弹。
// 手指下是哪一项按胶囊的项目区等分来算：各项的大小在变，但总长度不变，等分是稳定的，不会来回跳。

/** 胶囊的长按判断比系统默认（约 400ms）短，按住一下就能开始拖动。 */
internal const val PILL_LONG_PRESS_MS = 200L

/** 拖动的状态：[dragging] 正在长按拖动。 */
@Stable
internal class PillDrag {
    var dragging by mutableStateOf(false)
        internal set
}

@Composable
internal fun rememberPillDrag(): PillDrag = remember { PillDrag() }

/** 按住时胶囊放大的比例：1.04，松手回弹到 1。 */
@Composable
internal fun PillDrag.scale(): Float {
    val reduced = reducedMotion()
    val scale by animateFloatAsState(
        if (dragging && !reduced) 1.04f else 1f,
        if (reduced) snap() else spring(dampingRatio = 0.55f, stiffness = 500f),
        label = "press",
    )
    return scale
}

/** 胶囊里用短一些的长按时长，只影响胶囊。 */
@Composable
internal fun PillLongPress(content: @Composable () -> Unit) {
    val systemConfig = LocalViewConfiguration.current
    val pillConfig = remember(systemConfig) {
        object : ViewConfiguration by systemConfig {
            override val longPressTimeoutMillis: Long = PILL_LONG_PRESS_MS
        }
    }
    CompositionLocalProvider(LocalViewConfiguration provides pillConfig, content = content)
}

/**
 * 加在胶囊的项目区上：长按后拖动选项。[vertical] 为 true 时按上下等分（竖向），否则按左右（横向）。
 */
@Composable
internal fun Modifier.pillDrag(drag: PillDrag, count: Int, selected: Int, onSelect: (Int) -> Unit, vertical: Boolean = false): Modifier {
    val haptic = LocalHapticFeedback.current
    val currentCount by rememberUpdatedState(count)
    val currentSelected by rememberUpdatedState(selected)
    val currentSelect by rememberUpdatedState(onSelect)
    return pointerInput(vertical) {
        fun indexAt(at: Float): Int {
            val n = currentCount
            val length = if (vertical) size.height.toFloat() else size.width.toFloat()
            return (at / (length / n)).toInt().coerceIn(0, n - 1)
        }
        fun pick(at: Float) {
            val i = indexAt(at)
            if (i != currentSelected) {
                haptic.performHapticFeedback(HapticFeedbackType.TextHandleMove)
                currentSelect(i)
            }
        }
        detectDragGesturesAfterLongPress(
            onDragStart = { p ->
                drag.dragging = true
                haptic.performHapticFeedback(HapticFeedbackType.LongPress)
                pick(if (vertical) p.y else p.x)
            },
            onDrag = { change, _ -> pick(if (vertical) change.position.y else change.position.x) },
            onDragEnd = { drag.dragging = false },
            onDragCancel = { drag.dragging = false },
        )
    }
}
