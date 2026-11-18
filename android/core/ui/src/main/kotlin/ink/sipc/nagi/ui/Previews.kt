package ink.sipc.nagi.ui

import android.content.res.Configuration
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.tooling.preview.Preview
import androidx.compose.ui.unit.dp

// 组件预览：首页的大致样子，浅色和深色各一张，对照 android/design/s1.png。
@Preview(name = "浅色", widthDp = 360, heightDp = 760)
@Preview(name = "深色", widthDp = 360, heightDp = 760, uiMode = Configuration.UI_MODE_NIGHT_YES)
@Composable
private fun KitPreview() {
    NagiTheme {
        val c = NagiTheme.colors
        Box(Modifier.fillMaxSize().background(c.background)) {
            Column {
                NagiTopBar("家里的集群", onTitleClick = {}, showChevron = true) {
                    NagiIconButton(NagiIcons.Search, "搜索", {})
                }
                Column(Modifier.padding(horizontal = Dimens.pageHorizontal)) {
                    SectionHeader("需要处理") { Text("4", color = c.mutedForeground) }
                    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        StatusDot(c.warning)
                        Text("节点 node7 等待批准", Modifier.weight(1f))
                        NagiButton("批准", {}, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    }
                    SectionDivider()
                    SectionHeader("流量趋势") { UnderlineTabs(listOf("1 小时", "24 小时", "7 天"), 1, {}) }
                    ChartLegend(listOf("对外" to c.brand, "节点之间" to c.mutedForeground))
                    LineChart(
                        listOf(
                            ChartLine(listOf(3f, 5f, 4f, 6f, 5f, 7f, 6f, 8f), c.brand),
                            ChartLine(listOf(1f, 2f, 1.5f, 2.5f, 2f, 3f, 2.5f, 3f), c.mutedForeground),
                        ),
                        Modifier.padding(top = 12.dp),
                        xLabels = listOf("12:00", "18:00", "00:00", "06:00", "现在"),
                    )
                    UsageBar(0.6f, c.brand, Modifier.padding(top = 16.dp))
                    Row(Modifier.padding(top = 16.dp), horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        NagiButton("重启", {}, Modifier.weight(1f), variant = ButtonVariant.Outline)
                        NagiButton("部署", {}, Modifier.weight(1f), loading = true)
                    }
                    SkeletonRows(modifier = Modifier.padding(top = 16.dp))
                }
            }
            BottomFade(Modifier.align(Alignment.BottomCenter))
            PillNav(
                listOf(
                    PillNavItem(NagiIcons.Home, "首页", badge = true),
                    PillNavItem(NagiIcons.Folder, "项目"),
                    PillNavItem(NagiIcons.Server, "节点"),
                    PillNavItem(NagiIcons.Settings, "设置"),
                ),
                selected = 0,
                onSelect = {},
                modifier = Modifier.align(Alignment.BottomCenter),
            )
        }
    }
}
