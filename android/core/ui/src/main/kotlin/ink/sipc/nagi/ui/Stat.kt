package ink.sipc.nagi.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp

/**
 * StatValue：关键数字（见 docs/android-ui.md#关键数字）：上面灰色名称，下面大数字和灰色单位或分母（「12 / 14」「3.2 MB/s」）。
 * 数字变了只滚动变了的那几位，单位跟着让位。能点时（[onClick]）点了去对应的列表。
 */
@Composable
fun StatValue(label: String, value: String, modifier: Modifier = Modifier, suffix: String = "", onClick: (() -> Unit)? = null) {
    Column(modifier.then(if (onClick != null) Modifier.rowClickable(onClick) else Modifier)) {
        Text(label, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
        Spacer(Modifier.height(4.dp))
        Row(verticalAlignment = Alignment.Bottom) {
            Roll(value, NagiTheme.type.keyNumber, NagiTheme.colors.foreground)
            if (suffix.isNotEmpty()) {
                Roll(" $suffix", NagiTheme.type.secondary, NagiTheme.colors.mutedForeground, Modifier.padding(bottom = 3.dp))
            }
        }
    }
}
