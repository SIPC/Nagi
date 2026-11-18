package ink.sipc.nagi.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

// 提示和状态（见 docs/android-ui.md#提示和状态）。

/**
 * Banner：页面顶部的通栏提示（连不上面板、只读、版本过旧）。muted 底，状态点、标题、几行说明，最后是文字按钮。
 * 横向撑满屏幕，内容按页面左右 20 缩进。
 */
@Composable
fun Banner(
    title: String,
    modifier: Modifier = Modifier,
    lines: List<String> = emptyList(),
    dot: Color = NagiTheme.colors.warning,
    action: String? = null,
    onAction: () -> Unit = {},
) {
    val colors = NagiTheme.colors
    Row(modifier.fillMaxWidth().background(colors.muted).padding(horizontal = Dimens.pageHorizontal, vertical = 12.dp)) {
        Box(Modifier.padding(top = 7.dp)) { StatusDot(dot) }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(title, style = NagiTheme.type.bodyMedium, color = colors.foreground)
            lines.filter { it.isNotEmpty() }.forEach {
                Text(it, style = NagiTheme.type.secondary, color = colors.mutedForegroundStrong, maxLines = 2)
            }
            if (action != null) {
                Spacer(Modifier.height(4.dp))
                Text(
                    action, style = NagiTheme.type.bodyMedium, color = colors.foreground,
                    modifier = Modifier.clickable(onClick = onAction).padding(vertical = 8.dp),
                )
            }
        }
    }
}

/** InlineError：区块里的加载失败：红字说明，右边「重试」。[height] 和成功时的内容一样高，切换时不跳。 */
@Composable
fun InlineError(message: String, retryLabel: String, onRetry: () -> Unit, modifier: Modifier = Modifier, height: Dp? = null) {
    Row(
        modifier.fillMaxWidth().then(if (height != null) Modifier.height(height) else Modifier),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(message, style = NagiTheme.type.body, color = NagiTheme.colors.destructive, modifier = Modifier.weight(1f))
        NagiButton(retryLabel, onRetry, variant = ButtonVariant.Ghost, size = ButtonSize.Small)
    }
}

/** EmptyState：整页或整块没有内容时居中的一行灰字，下面可以放一个按钮。 */
@Composable
fun EmptyState(text: String, modifier: Modifier = Modifier, action: (@Composable () -> Unit)? = null) {
    Box(modifier.fillMaxSize().padding(horizontal = Dimens.pageHorizontal), contentAlignment = Alignment.Center) {
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            Text(text, style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground, textAlign = TextAlign.Center)
            if (action != null) {
                Spacer(Modifier.height(16.dp))
                action()
            }
        }
    }
}
