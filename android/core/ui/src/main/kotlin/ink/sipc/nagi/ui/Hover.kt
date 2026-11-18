package ink.sipc.nagi.ui

import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.input.pointer.PointerEventType
import androidx.compose.ui.input.pointer.PointerType
import androidx.compose.ui.input.pointer.pointerInput

/**
 * 鼠标悬浮（平板接鼠标、触控板）：指针在上面移动时 [onMove]（像素坐标），移出时 [onExit]。
 * 只认鼠标，手指和触控笔不管，那些照常走点按和长按。
 */
fun Modifier.mouseHover(onMove: (Offset) -> Unit, onExit: () -> Unit): Modifier = pointerInput(Unit) {
    awaitPointerEventScope {
        while (true) {
            val e = awaitPointerEvent()
            val c = e.changes.firstOrNull() ?: continue
            if (c.type != PointerType.Mouse) continue
            when (e.type) {
                PointerEventType.Enter, PointerEventType.Move -> onMove(c.position)
                PointerEventType.Exit -> onExit()
            }
        }
    }
}
