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
    val pillHeight = 48.dp
    val pillItemHeight = 40.dp
    // 胶囊后面的渐隐高度，也是页面底部要留的空白
    val bottomFade = 96.dp
}
