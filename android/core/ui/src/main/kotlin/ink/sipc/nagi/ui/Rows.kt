package ink.sipc.nagi.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

// 列表行（见 docs/android-ui.md#列表行）。

/** 行的高度：普通 48 / 56（两行），紧凑 44 / 52（首页「需要处理」）。 */
enum class RowDensity(val oneLine: Dp, val twoLine: Dp) {
    Normal(Dimens.rowHeight, Dimens.rowHeightTwoLine),
    Compact(Dimens.attentionRow, Dimens.attentionRowTwoLine),
}

/**
 * ListRow：一行列表项。左边是状态点（[dot]，和第一行文字对齐），中间标题（最多两行）和说明（一行），右边放控件
 * （小按钮、数量），没有控件又能点时显示「›」。整行可点（[onClick]）。
 */
@Composable
fun ListRow(
    title: String,
    modifier: Modifier = Modifier,
    detail: String = "",
    dot: Color? = null,
    density: RowDensity = RowDensity.Normal,
    onClick: (() -> Unit)? = null,
    trailing: (@Composable RowScope.() -> Unit)? = null,
) {
    val colors = NagiTheme.colors
    val twoLine = detail.isNotEmpty()
    Row(
        modifier.fillMaxWidth()
            .then(if (onClick != null) Modifier.rowClickable(onClick) else Modifier)
            .heightIn(min = if (twoLine) density.twoLine else density.oneLine)
            .padding(vertical = 6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (dot != null) {
            // 两行时点和第一行对齐。
            Box(Modifier.align(if (twoLine) Alignment.Top else Alignment.CenterVertically).padding(top = if (twoLine) 8.dp else 0.dp)) {
                StatusDot(dot)
            }
            Spacer(Modifier.width(DotGap))
        }
        Column(Modifier.weight(1f)) {
            Text(title, style = NagiTheme.type.body, color = colors.foreground, maxLines = 2, overflow = TextOverflow.Ellipsis)
            if (twoLine) Text(detail, style = NagiTheme.type.secondary, color = colors.mutedForeground, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        when {
            trailing != null -> {
                Spacer(Modifier.width(12.dp))
                Row(verticalAlignment = Alignment.CenterVertically, content = trailing)
            }
            onClick != null -> {
                Spacer(Modifier.width(12.dp))
                Icon(NagiIcons.ChevronRight, contentDescription = null, tint = colors.mutedForeground, modifier = Modifier.size(16.dp))
            }
        }
    }
}

/** MoreRow：列表折叠后的「还有 N 项」，文字和上面带状态点的行的标题对齐。 */
@Composable
fun MoreRow(text: String, onClick: () -> Unit, modifier: Modifier = Modifier, density: RowDensity = RowDensity.Normal, indent: Boolean = true) {
    Row(
        modifier.fillMaxWidth().rowClickable(onClick).heightIn(min = density.oneLine),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (indent) Spacer(Modifier.width(6.dp + DotGap))
        Text(text, style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground, modifier = Modifier.weight(1f))
    }
}

/** KeyValueRow：名称和值（核对信息、详情页的属性）。名称固定宽度左对齐，[mono] 时值用等宽字（地址、指纹、ID）。 */
@Composable
fun KeyValueRow(label: String, value: String, modifier: Modifier = Modifier, mono: Boolean = false, labelWidth: Dp = 48.dp) {
    Row(modifier.fillMaxWidth().padding(vertical = 6.dp), horizontalArrangement = Arrangement.spacedBy(16.dp)) {
        Text(label, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.width(labelWidth))
        Text(value, style = if (mono) NagiTheme.type.mono else NagiTheme.type.body, color = NagiTheme.colors.foreground)
    }
}

/** UsageRow：名称、右边的数值，下面一条用量条（排行、磁盘占用）。数值变化时跳字。 */
@Composable
fun UsageRow(
    label: String,
    value: String,
    fraction: Float,
    modifier: Modifier = Modifier,
    mono: Boolean = false,
    color: Color = NagiTheme.colors.brand,
) {
    Column(modifier) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(
                label, style = if (mono) NagiTheme.type.mono else NagiTheme.type.body, color = NagiTheme.colors.foreground,
                maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f),
            )
            Spacer(Modifier.width(12.dp))
            Roll(value, NagiTheme.type.secondary, NagiTheme.colors.mutedForeground)
        }
        Spacer(Modifier.height(6.dp))
        UsageBar(fraction, color, Modifier.fillMaxWidth())
    }
}

/** 状态点和文字之间的间隔。 */
private val DotGap = 12.dp
