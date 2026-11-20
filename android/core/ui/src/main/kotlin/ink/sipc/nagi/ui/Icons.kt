package ink.sipc.nagi.ui

import androidx.compose.foundation.Image
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.graphics.vector.addPathNodes
import androidx.compose.ui.graphics.vector.rememberVectorPainter
import androidx.compose.ui.unit.dp

// 图标：Lucide 的描边图标（24 × 24，线宽 2，圆头圆角），路径取自 lucide-react，和网页同一套。
// 圆和圆角矩形换成路径命令。颜色由 Icon 的 tint 决定。
object NagiIcons {
    // 主导航四项和网页的主导航同一套（web/src/components/nav-icons.tsx）。
    val Home: ImageVector by lazy {
        icon(
            "house",
            "M15 21v-8a1 1 0 0 0-1-1h-4a1 1 0 0 0-1 1v8",
            "M3 10a2 2 0 0 1 .709-1.528l7-6a2 2 0 0 1 2.582 0l7 6A2 2 0 0 1 21 10v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z",
        )
    }
    val Folder: ImageVector by lazy {
        icon("folder", "M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z")
    }
    val Server: ImageVector by lazy {
        icon("server", rect(2f, 2f, 20f, 8f, 2f), rect(2f, 14f, 20f, 8f, 2f), "M6 6h.01", "M6 18h.01")
    }
    val Settings: ImageVector by lazy {
        icon(
            "settings",
            "M9.671 4.136a2.34 2.34 0 0 1 4.659 0 2.34 2.34 0 0 0 3.319 1.915 2.34 2.34 0 0 1 2.33 4.033 2.34 2.34 0 0 0 0 3.831 2.34 2.34 0 0 1-2.33 4.033 2.34 2.34 0 0 0-3.319 1.915 2.34 2.34 0 0 1-4.659 0 2.34 2.34 0 0 0-3.32-1.915 2.34 2.34 0 0 1-2.33-4.033 2.34 2.34 0 0 0 0-3.831A2.34 2.34 0 0 1 6.35 6.051a2.34 2.34 0 0 0 3.319-1.915",
            circle(12f, 12f, 3f),
        )
    }
    val Search: ImageVector by lazy { icon("search", "m21 21-4.34-4.34", circle(11f, 11f, 8f)) }
    val ChevronDown: ImageVector by lazy { icon("chevron-down", "m6 9 6 6 6-6") }
    val ChevronRight: ImageVector by lazy { icon("chevron-right", "m9 18 6-6-6-6") }
    val ArrowLeft: ImageVector by lazy { icon("arrow-left", "m12 19-7-7 7-7", "M19 12H5") }
    val Lock: ImageVector by lazy { icon("lock", rect(3f, 11f, 18f, 11f, 2f), "M7 11V7a5 5 0 0 1 10 0v4") }
    val Plus: ImageVector by lazy { icon("plus", "M5 12h14", "M12 5v14") }
    // 节点的占用环中间的图标（CPU、内存、磁盘）。
    val Cpu: ImageVector by lazy {
        icon(
            "cpu",
            "M12 20v2", "M12 2v2", "M17 20v2", "M17 2v2", "M2 12h2", "M2 17h2", "M2 7h2", "M20 12h2", "M20 17h2", "M20 7h2", "M7 20v2", "M7 2v2",
            rect(4f, 4f, 16f, 16f, 2f), rect(8f, 8f, 8f, 8f, 1f),
        )
    }
    val MemoryStick: ImageVector by lazy {
        icon(
            "memory-stick",
            "M12 12v-2", "M12 18v-2", "M16 12v-2", "M16 18v-2", "M2 11h1.5", "M20 18v-2", "M20.5 11H22", "M4 18v-2", "M8 12v-2", "M8 18v-2",
            rect(2f, 6f, 20f, 10f, 2f),
        )
    }
    val HardDrive: ImageVector by lazy {
        icon(
            "hard-drive",
            "M10 16h.01",
            "M2.212 11.577a2 2 0 0 0-.212.896V18a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-5.527a2 2 0 0 0-.212-.896L18.55 5.11A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z",
            "M21.946 12.013H2.054", "M6 16h.01",
        )
    }
    val Check: ImageVector by lazy { icon("check", "M20 6 9 17l-5-5") }
    val Share: ImageVector by lazy { icon("share-2", circle(18f, 5f, 3f), circle(6f, 12f, 3f), circle(18f, 19f, 3f), "m8.59 13.51 6.83 3.98", "m15.41 6.51-6.82 3.98") }

    // 扫码：lucide scan-line
    val ScanQr: ImageVector by lazy {
        icon(
            "scan-line",
            "M3 7V5a2 2 0 0 1 2-2h2",
            "M17 3h2a2 2 0 0 1 2 2v2",
            "M21 17v2a2 2 0 0 1-2 2h-2",
            "M7 21H5a2 2 0 0 1-2-2v-2",
            "M7 12h10",
        )
    }
    // 文件管理（lucide file、x）
    val File: ImageVector by lazy { icon("file", "M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z", "M14 2v4a2 2 0 0 0 2 2h4") }
    val X: ImageVector by lazy { icon("x", "M18 6 6 18", "m6 6 12 12") }
    val Copy: ImageVector by lazy { icon("copy", rect(8f, 8f, 14f, 14f, 2f), "M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2") }

    // 二级页面顶部栏的 ⋯（lucide ellipsis）
    val More: ImageVector by lazy { icon("ellipsis", circle(12f, 12f, 1f), circle(19f, 12f, 1f), circle(5f, 12f, 1f)) }

    // Logo 的单色版本，可以着色（上面一道 55% 不透明）。要品牌色用 NagiLogo。
    val Logo: ImageVector by lazy { logo(Color.Black, Color.Black) }
}

// NagiLogo：两道波浪，和网页的 Waves 相同：下面一道品牌色，上面一道前景色、55% 不透明。默认 24 × 14。
@Composable
fun NagiLogo(modifier: Modifier = Modifier) {
    val c = NagiTheme.colors
    val vector = remember(c.brand, c.foreground) { logo(c.brand, c.foreground) }
    Image(rememberVectorPainter(vector), contentDescription = "Nagi", modifier = modifier)
}

// 网页的 viewBox 是 "4 9 24 14"，这里把路径平移到原点。
private fun logo(wave: Color, top: Color): ImageVector =
    ImageVector.Builder("nagi-logo", 24.dp, 14.dp, 24f, 14f)
        .addPath(
            addPathNodes("M2 10c3-3 6-3 10 0s7 3 10 0"),
            stroke = SolidColor(wave),
            strokeLineWidth = 2.4f,
            strokeLineCap = StrokeCap.Round,
        )
        .addPath(
            addPathNodes("M2 4c3-3 6-3 10 0s7 3 10 0"),
            stroke = SolidColor(top),
            strokeAlpha = 0.55f,
            strokeLineWidth = 2.4f,
            strokeLineCap = StrokeCap.Round,
        )
        .build()

private fun icon(name: String, vararg paths: String): ImageVector {
    val b = ImageVector.Builder(name, 24.dp, 24.dp, 24f, 24f)
    for (d in paths) {
        b.addPath(
            addPathNodes(d),
            fill = null,
            stroke = SolidColor(Color.Black),
            strokeLineWidth = 2f,
            strokeLineCap = StrokeCap.Round,
            strokeLineJoin = StrokeJoin.Round,
        )
    }
    return b.build()
}

// 圆角矩形（SVG rect 的 x、y、width、height、rx）转成路径。
private fun rect(x: Float, y: Float, w: Float, h: Float, r: Float): String {
    val iw = w - 2 * r
    val ih = h - 2 * r
    return "M${x + r} ${y}h${iw}a$r $r 0 0 1 $r ${r}v${ih}a$r $r 0 0 1 -$r ${r}h-${iw}a$r $r 0 0 1 -$r -${r}v-${ih}a$r $r 0 0 1 $r -${r}z"
}

// 圆转成两段半圆弧。
private fun circle(cx: Float, cy: Float, r: Float): String =
    "M${cx - r} ${cy}a$r $r 0 1 0 ${2 * r} 0a$r $r 0 1 0 -${2 * r} 0z"
