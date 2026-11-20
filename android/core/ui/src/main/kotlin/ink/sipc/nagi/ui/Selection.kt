package ink.sipc.nagi.ui

import androidx.compose.animation.AnimatedContent
import androidx.compose.foundation.layout.Box
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Stable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Rect
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.ContentDrawScope
import androidx.compose.ui.layout.LayoutCoordinates
import androidx.compose.ui.layout.onGloballyPositioned
import androidx.compose.ui.node.CompositionLocalConsumerModifierNode
import androidx.compose.ui.node.DrawModifierNode
import androidx.compose.ui.node.GlobalPositionAwareModifierNode
import androidx.compose.ui.node.ModifierNodeElement
import androidx.compose.ui.node.currentValueOf
import androidx.compose.ui.node.invalidateDraw
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.unit.dp

// 列表里选中一行的底色（宽屏设置页左边的菜单、宽屏节点页左边的节点列表）：从上一行滑过去，
// 朝移动方向的边先到、另一条边跟上，和胶囊、标签的选中指示器同一套（SlidingIndicator，docs/ui.md「动画」）。
// 用法：列表外面包一层 SlidingSelection，可选的行用 rowClickable(selectionKey = …, highlight = …)，选中的一行传 highlight。
// 外面没有 SlidingSelection 时，选中的行照常自己画一块不动的底色。

/** 选中底色的状态：各行的位置（相对于外面那一层）和当前选中的是哪一行。 */
@Stable
class SelectionState internal constructor() {
    internal var frame: LayoutCoordinates? = null
    private val rows = HashMap<Any, LayoutCoordinates>()
    private val ids = HashMap<Any, Int>()
    internal val rects = mutableStateMapOf<Int, Rect>()
    internal var selected by mutableStateOf<Any?>(null)

    internal fun idOf(key: Any): Int = ids.getOrPut(key) { ids.size }

    internal fun place(key: Any, coords: LayoutCoordinates) {
        rows[key] = coords
        update(key)
    }

    internal fun mark(key: Any, on: Boolean) {
        if (on) selected = key else if (selected == key) selected = null
    }

    internal fun remove(key: Any) {
        rows.remove(key)
        rects.remove(idOf(key))
        if (selected == key) selected = null
    }

    internal fun updateAll() = rows.keys.toList().forEach(::update)

    /** 列表里有没有 [key] 这一行。 */
    fun has(key: Any?): Boolean = key != null && rows.containsKey(key)

    private fun update(key: Any) {
        val f = frame ?: return
        val c = rows[key] ?: return
        if (f.isAttached && c.isAttached) rects[idOf(key)] = f.localBoundingBoxOf(c, clipBounds = false)
    }

    /** [key] 那一行是不是在当前选中的一行下面：右边的内容按这个方向滑入。不知道时算往下。 */
    fun isBelowSelected(key: Any): Boolean {
        val to = rects[idOf(key)] ?: return true
        val from = selected?.let { rects[idOf(it)] } ?: return true
        return to.center.y >= from.center.y
    }
}

@Composable
fun rememberSelectionState(): SelectionState = remember { SelectionState() }

internal val LocalSelection = staticCompositionLocalOf<SelectionState?> { null }

/** 包住一个列表：里面用 selectionKey 的行，选中底色由这一层画，换一行时从上一行滑过去。 */
@Composable
fun SlidingSelection(state: SelectionState, modifier: Modifier = Modifier, content: @Composable () -> Unit) {
    val c = NagiTheme.colors
    val selected = state.selected
    val slider = rememberSlidingIndicator(selected?.let(state::idOf) ?: -1)
    val radius = with(LocalDensity.current) { 8.dp.toPx() }
    Box(
        modifier
            .onGloballyPositioned { state.frame = it; state.updateAll() }
            .drawBehind {
                if (state.selected == null) return@drawBehind
                val r = slider.rect(state.rects, vertical = true) ?: return@drawBehind
                drawRoundRect(c.muted, r.topLeft, r.size, CornerRadius(radius))
            },
    ) {
        CompositionLocalProvider(LocalSelection provides state, content = content)
    }
}

/**
 * 一行的选中底色：外面有 [SlidingSelection] 且有 [key] 时登记这一行的位置、选中时告诉外面，底色由外面画；
 * 否则选中时（[highlight] 不为 null）自己画。
 */
internal fun Modifier.selectionRow(key: Any?, highlight: Color?): Modifier = this then SelectionRowElement(key, highlight)

private data class SelectionRowElement(val key: Any?, val highlight: Color?) : ModifierNodeElement<SelectionRowNode>() {
    override fun create() = SelectionRowNode(key, highlight)
    override fun update(node: SelectionRowNode) = node.update(key, highlight)
}

private class SelectionRowNode(var key: Any?, var highlight: Color?) :
    Modifier.Node(), CompositionLocalConsumerModifierNode, GlobalPositionAwareModifierNode, DrawModifierNode {
    private var state: SelectionState? = null
    private val managed get() = state != null && key != null

    override fun onAttach() {
        state = currentValueOf(LocalSelection)
        key?.let { state?.mark(it, highlight != null) }
    }

    override fun onDetach() {
        key?.let { state?.remove(it) }
        state = null
    }

    fun update(key: Any?, highlight: Color?) {
        if (key != this.key) this.key?.let { state?.remove(it) }
        this.key = key
        this.highlight = highlight
        key?.let { state?.mark(it, highlight != null) }
        invalidateDraw()
    }

    override fun onGloballyPositioned(coordinates: LayoutCoordinates) {
        val k = key ?: return
        state?.place(k, coordinates)
    }

    override fun ContentDrawScope.draw() {
        val h = highlight
        if (h != null && !managed) drawRect(h)
        drawContent()
    }
}

/**
 * 页内标签下面的内容：换标签时跟标签同方向滑入（选右边的，新内容从右边进来 32、旧内容往左移开 16 并淡出），
 * 和网页「点导航、标签、切换控件后的内容」一样。「移除动画」时只淡入淡出。
 */
@Composable
fun PageTabContent(selected: Int, modifier: Modifier = Modifier, content: @Composable (Int) -> Unit) {
    val reduced = reducedMotion()
    val density = LocalDensity.current
    val inPx = with(density) { 32.dp.roundToPx() }
    val outPx = with(density) { 16.dp.roundToPx() }
    AnimatedContent(
        targetState = selected,
        modifier = modifier,
        transitionSpec = { NagiTransitions.tab(targetState > initialState, inPx, outPx, reduced) },
        label = "page-tab",
    ) { content(it) }
}
