package ink.sipc.nagi.ui

import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.input.pointer.PointerEventType
import androidx.compose.ui.input.pointer.pointerInput

/**
 * 悬浮（平板接鼠标、触控板，或触控笔悬空）：指针没按下、在上面移动时 [onMove]（像素坐标），移出时 [onExit]。
 * 不看指针类型：触控板移动指针时报的是手指（TOOL_TYPE_FINGER），按类型只认鼠标会漏掉触控板和触控笔。
 * 手指在屏幕上点按时指针一直是按下的，不算悬浮，照常走点按和长按。
 */
fun Modifier.mouseHover(onMove: (Offset) -> Unit, onExit: () -> Unit): Modifier = pointerInput(Unit) {
    awaitPointerEventScope {
        var hovering = false
        while (true) {
            val e = awaitPointerEvent()
            val c = e.changes.firstOrNull() ?: continue
            when (e.type) {
                PointerEventType.Enter, PointerEventType.Move -> if (!c.pressed) { hovering = true; onMove(c.position) }
                PointerEventType.Exit -> if (hovering) { hovering = false; onExit() }
            }
        }
    }
}
