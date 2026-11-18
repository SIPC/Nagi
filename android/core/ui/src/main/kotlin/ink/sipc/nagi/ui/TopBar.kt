package ink.sipc.nagi.ui

import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.draw.drawBehind
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.collectIsPressedAsState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp

private val TopBarHeight = Dimens.topBarHeight

// 右侧内边距 6：最后一个 48 宽的图标按钮，图形右边缘正好离屏幕边 20。
private val TopBarEnd = 6.dp

// 顶部栏最下面渐变成透明的那一段。
private val TopBarFade = 20.dp

// NagiTopBar：一级页面的顶部栏。左边是标题（面板名称）和下拉箭头，右边是新建、搜索。高 56，自带状态栏的内边距。
// 顶部栏到内容的 4（Dimens.topBarToContent）由页面自己留。
// fade：内容从顶部栏下面滚过时用。顶部栏自己画背景，上面是背景色，最下面 TopBarFade 渐变成透明，内容滚到那里变淡；
// 渐变收在顶部栏里面，不往内容区伸。
@Composable
fun NagiTopBar(
    title: String,
    modifier: Modifier = Modifier,
    onTitleClick: (() -> Unit)? = null,
    showChevron: Boolean = false,
    fade: Boolean = false,
    actions: @Composable RowScope.() -> Unit = {},
) {
    val bg = NagiTheme.colors.background
    Row(
        modifier = modifier
            .fillMaxWidth()
            .then(
                if (fade) {
                    Modifier.drawBehind {
                        val start = (size.height - TopBarFade.toPx()) / size.height
                        drawRect(Brush.verticalGradient(0f to bg, start to bg, 1f to bg.copy(alpha = 0f)))
                    }
                } else {
                    Modifier
                },
            )
            .statusBarsPadding()
            .height(TopBarHeight)
            .padding(start = Dimens.pageHorizontal, end = TopBarEnd),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.weight(1f).fillMaxHeight(), contentAlignment = Alignment.CenterStart) {
            val interaction = remember { MutableInteractionSource() }
            val pressed by interaction.collectIsPressedAsState()
            Row(
                modifier = if (onTitleClick != null) {
                    Modifier.height(Dimens.iconButton).pressable(true, Role.Button, null, interaction, pressed, onTitleClick)
                } else {
                    Modifier
                },
                horizontalArrangement = Arrangement.spacedBy(4.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    title,
                    style = NagiTheme.type.topBar,
                    color = NagiTheme.colors.foreground,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f, fill = false).semantics { heading() },
                )
                if (showChevron) {
                    Icon(NagiIcons.ChevronDown, contentDescription = null, tint = NagiTheme.colors.foreground, modifier = Modifier.size(16.dp))
                }
            }
        }
        actions()
    }
}

// NagiBackTopBar：二级页面的顶部栏：返回、标题（后面可跟一段等宽的副标题，如容器页的「web orders-web」）、⋯。
@Composable
fun NagiBackTopBar(
    title: String,
    onBack: () -> Unit,
    modifier: Modifier = Modifier,
    subtitle: String? = null,
    backDescription: String = "返回",
    actions: @Composable RowScope.() -> Unit = {},
) {
    Row(
        modifier = modifier
            .fillMaxWidth()
            .statusBarsPadding()
            .height(TopBarHeight)
            .padding(start = TopBarEnd, end = TopBarEnd),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        NagiIconButton(NagiIcons.ArrowLeft, backDescription, onBack)
        Row(
            modifier = Modifier.weight(1f),
            horizontalArrangement = Arrangement.spacedBy(6.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(
                title,
                style = NagiTheme.type.topBar,
                color = NagiTheme.colors.foreground,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.weight(1f, fill = false).semantics { heading() },
            )
            if (subtitle != null) {
                Text(
                    subtitle,
                    style = NagiTheme.type.mono,
                    color = NagiTheme.colors.mutedForeground,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f, fill = false),
                )
            }
        }
        actions()
    }
}
