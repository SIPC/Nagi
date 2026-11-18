package io.github.sipc.nagi.ui

import java.util.Locale

// 字节数和速率，和网页 components/metrics.tsx 的 sizeParts 同一套写法（docs/ui.md 文案）：
// 到 1000 就进位（按 1024 换算，不出现 1010 MB 这样的 4 位数）。
// 容量不到 100 保留一位小数：512 B、1.4 GB、15.6 GB、156 GB；
// 速率变化快，不到 10 才保留一位小数：640 KB/s、89 KB/s、1.2 MB/s。B 和 B/s 不带小数。
// 返回数字和单位，分开排版（数字大、单位小）。

private val sizeUnits = listOf("B", "KB", "MB", "GB", "TB")
private val rateUnits = listOf("B/s", "KB/s", "MB/s", "GB/s", "TB/s")

private fun sizeParts(value: Double, units: List<String>, decimalsBelow: Double): Pair<String, String> {
    var v = value
    var i = 0
    while (v >= 1000 && i < units.size - 1) {
        v /= 1024
        i++
    }
    val n = if (v < decimalsBelow && i > 0) String.format(Locale.ROOT, "%.1f", v) else Math.round(v).toString()
    return n to units[i]
}

// formatRate：速率，例如 1.2 to "MB/s"。
fun formatRate(bytesPerSecond: Double): Pair<String, String> = sizeParts(bytesPerSecond, rateUnits, 10.0)

// formatBytes：字节数，例如 15.6 to "GB"。
fun formatBytes(bytes: Double): Pair<String, String> = sizeParts(bytes, sizeUnits, 100.0)

// 连在一起的写法，例如「1.2 MB/s」「15.6 GB」。
fun formatRateText(bytesPerSecond: Double): String = formatRate(bytesPerSecond).let { "${it.first} ${it.second}" }

fun formatBytesText(bytes: Double): String = formatBytes(bytes).let { "${it.first} ${it.second}" }
