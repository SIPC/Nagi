package ink.sipc.nagi.data

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

// 节点上报的数先过 metric（安全审查 v0.35.0#16）：NaN、Inf、负数不进首页的图表。
class MetricTest {
    @Test
    fun nonFiniteAndNegativeBecomeZero() {
        for (v in listOf(Double.NaN, Double.POSITIVE_INFINITY, Double.NEGATIVE_INFINITY, -1.0, -0.0, 0.0)) {
            assertEquals("$v", 0.0, metric(v), 0.0)
        }
    }

    @Test
    fun normalValuesPassAndHugeOnesAreCapped() {
        assertEquals(1234.5, metric(1234.5), 0.0)
        assertEquals(MAX_METRIC, metric(1e300), 0.0)
        assertEquals(MAX_METRIC, metric(Double.MAX_VALUE), 0.0)
        // 截过的数加很多次也还是有限的。
        assertTrue((1..100_000).sumOf { metric(Double.MAX_VALUE) }.isFinite())
    }
}
