package ink.sipc.nagi.ui

import org.junit.Assert.assertEquals
import org.junit.Test

// 桑基图和数字格式对非有限的速率兜底（安全审查 v0.35.0#16）。
class SankeyTest {
    @Test
    fun dropsNonFiniteAndClampsNegative() {
        val flows = listOf(
            SankeyFlow("a", "b", Double.NaN, FlowKind.Mesh),
            SankeyFlow("a", SANKEY_EXTERNAL, Double.POSITIVE_INFINITY, FlowKind.External),
            SankeyFlow(SANKEY_EXTERNAL, "a", -5.0, FlowKind.External),
            SankeyFlow("b", "a", 2048.0, FlowKind.Mesh),
        )
        val out = finiteFlows(flows)
        assertEquals(listOf(0.0, 2048.0), out.map { it.rate })
        assertEquals(listOf(SANKEY_EXTERNAL, "b"), out.map { it.from })
    }

    @Test
    fun formatTreatsNonFiniteAsZero() {
        assertEquals("0 B/s", formatRateText(Double.NaN))
        assertEquals("0 B/s", formatRateText(Double.POSITIVE_INFINITY))
        assertEquals("0 B", formatBytesText(-1.0))
        assertEquals("1.5 KB/s", formatRateText(1536.0))
    }
}
