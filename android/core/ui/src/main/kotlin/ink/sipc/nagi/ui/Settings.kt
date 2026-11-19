package ink.sipc.nagi.ui

import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp

// 设置页的组和行（见 docs/android-ui.md#设置）。各组之间 24，组标题到第一行 4（docs/android.md 视觉「间距」）。

/** SettingsGroup：一组设置，上面是灰色的组标题（[title] 为 null 时不写，只留间距）。 */
@Composable
fun SettingsGroup(title: String?, modifier: Modifier = Modifier, content: @Composable ColumnScope.() -> Unit) {
    Column(modifier.fillMaxWidth().padding(top = 24.dp)) {
        if (title != null) {
            Text(
                title, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground,
                modifier = Modifier.padding(bottom = 4.dp).semantics { heading() },
            )
        }
        content()
    }
}

/** SettingsRow：一行设置，左边名称，右边当前值（灰色）和「›」，点了进下一级。 */
@Composable
fun SettingsRow(title: String, onClick: () -> Unit, modifier: Modifier = Modifier, value: String? = null, selected: Boolean = false, selectionKey: Any? = null) {
    Row(
        modifier.fillMaxWidth().rowClickable(onClick, highlight = if (selected) NagiTheme.colors.muted else null, selectionKey = selectionKey).heightIn(min = Dimens.rowHeight),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(title, style = NagiTheme.type.body, color = NagiTheme.colors.foreground, modifier = Modifier.weight(1f))
        if (value != null) {
            Spacer(Modifier.width(12.dp))
            Text(
                value, style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground, maxLines = 1,
                // 值靠右，太长时截断，最多占这一行的一半左右。
                overflow = TextOverflow.Ellipsis, textAlign = TextAlign.End, modifier = Modifier.widthIn(max = 180.dp),
            )
        }
        Spacer(Modifier.width(8.dp))
        Icon(NagiIcons.ChevronRight, contentDescription = null, tint = NagiTheme.colors.mutedForeground, modifier = Modifier.size(16.dp))
    }
}

/** ChoiceRow：单选列表的一项（外观、语言），选中的一项右边打勾。 */
@Composable
fun ChoiceRow(title: String, selected: Boolean, onClick: () -> Unit, modifier: Modifier = Modifier) {
    Row(
        modifier.fillMaxWidth()
            .semantics { this.selected = selected }
            .rowClickable(onClick, role = Role.RadioButton)
            .heightIn(min = Dimens.rowHeight),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(title, style = NagiTheme.type.body, color = NagiTheme.colors.foreground, modifier = Modifier.weight(1f))
        if (selected) Icon(NagiIcons.Check, contentDescription = null, tint = NagiTheme.colors.foreground, modifier = Modifier.size(18.dp))
    }
}

/**
 * ActionRow：和网页的 SettingRow 一样，左边名称和灰色说明，右边最多两个按钮（有框的小按钮）。不整行可点。
 * 手机上说明长时换行，按钮靠右和名称那一行对齐。
 */
@Composable
fun ActionRow(title: String, modifier: Modifier = Modifier, description: String? = null, actions: @Composable RowScope.() -> Unit = {}) {
    Row(modifier.fillMaxWidth().heightIn(min = Dimens.rowHeight).padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text(title, style = NagiTheme.type.body, color = NagiTheme.colors.foreground)
            if (!description.isNullOrEmpty()) Text(description, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
        }
        Spacer(Modifier.width(12.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically, content = actions)
    }
}

/**
 * SwitchRow：开关写成设置行（docs/android.md 视觉「开关」）：第二行开头写「已开启」或「已关闭」，后面可以跟说明；
 * 右边一个有框的小按钮「关闭」或「开启」，不用安卓的开关控件。
 */
@Composable
fun SwitchRow(
    title: String,
    on: Boolean,
    onToggle: () -> Unit,
    labels: SwitchLabels,
    modifier: Modifier = Modifier,
    description: String? = null,
    busy: Boolean = false,
    enabled: Boolean = true,
) {
    val status = if (on) labels.onStatus else labels.offStatus
    ActionRow(title, modifier, listOfNotNull(status, description?.takeIf { it.isNotEmpty() }).joinToString(" · ")) {
        NagiButton(if (on) labels.turnOff else labels.turnOn, onToggle, variant = ButtonVariant.Outline, size = ButtonSize.Small, loading = busy, enabled = enabled)
    }
}

/** SwitchRow 的四段文字（从各页面的字符串资源取）。 */
class SwitchLabels(val onStatus: String, val offStatus: String, val turnOn: String, val turnOff: String)
