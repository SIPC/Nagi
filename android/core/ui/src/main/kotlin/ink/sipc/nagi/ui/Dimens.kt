package ink.sipc.nagi.ui

import androidx.compose.ui.unit.dp

// 尺寸和间距，取值同 docs/android.md 视觉。
object Dimens {
    val pageHorizontal = 20.dp
    val topBarToContent = 4.dp
    // 大区块之间一条细线，线上下各 16
    val sectionGap = 16.dp
    val sectionTitleHeight = 36.dp
    val cardPadding = 16.dp
    val buttonHeight = 36.dp
    val smallButtonHeight = 32.dp
    val rowHeight = 48.dp
    val rowHeightTwoLine = 56.dp
    // 首页「需要处理」更紧
    val attentionRow = 44.dp
    val attentionRowTwoLine = 52.dp
    val iconButton = 48.dp
    val pillHeight = 52.dp
    val pillItemHeight = 44.dp
    // 胶囊占的高度（连同离手势条的距离），页面底部要留出这么多空白，内容才不被胶囊挡住
    val bottomFade = 104.dp
    // 顶部栏的高度（不含状态栏）
    val topBarHeight = 56.dp
}
