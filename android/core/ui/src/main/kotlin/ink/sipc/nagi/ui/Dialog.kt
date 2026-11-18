package ink.sipc.nagi.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.shadow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties

/**
 * NagiDialog：确认框，和网页的确认对话框一样：标题、说明，下面是「取消」和操作按钮（并排，各占一半）。
 * [danger] 时操作按钮是红色（删除、退出、吊销）。[content] 放在说明下面（输入框、错误）。[busy] 时操作按钮转圈、不能关。
 */
@Composable
fun NagiDialog(
    title: String,
    confirm: String,
    cancel: String,
    onConfirm: () -> Unit,
    onDismiss: () -> Unit,
    text: String? = null,
    danger: Boolean = false,
    busy: Boolean = false,
    confirmEnabled: Boolean = true,
    content: @Composable ColumnScope.() -> Unit = {},
) {
    Dialog(onDismissRequest = { if (!busy) onDismiss() }, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        val shape = RoundedCornerShape(12.dp)
        Column(
            Modifier.padding(horizontal = 24.dp).widthIn(max = 420.dp).fillMaxWidth()
                .shadow(16.dp, shape).background(NagiTheme.colors.card, shape).padding(20.dp),
        ) {
            Text(title, style = NagiTheme.type.section, color = NagiTheme.colors.foreground)
            if (text != null) {
                Spacer(Modifier.height(8.dp))
                Text(text, style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground)
            }
            content()
            Spacer(Modifier.height(20.dp))
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                NagiButton(cancel, onDismiss, Modifier.weight(1f), variant = ButtonVariant.Outline, enabled = !busy)
                NagiButton(
                    confirm, onConfirm, Modifier.weight(1f),
                    variant = if (danger) ButtonVariant.Destructive else ButtonVariant.Primary, loading = busy, enabled = confirmEnabled,
                )
            }
        }
    }
}
