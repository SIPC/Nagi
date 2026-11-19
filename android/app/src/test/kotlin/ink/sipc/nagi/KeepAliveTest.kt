package ink.sipc.nagi

import org.junit.Assert.assertEquals
import org.junit.Test

// 前台换令牌的间隔：离开前台的时间也算在内。
class KeepAliveTest {
    @Test
    fun waitsTheRestOfTheInterval() {
        assertEquals(KEEP_ALIVE_MS, keepAliveWait(now = 1_000, last = 1_000))
        assertEquals(KEEP_ALIVE_MS - 90_000, keepAliveWait(now = 91_000, last = 1_000))
    }

    @Test
    fun overdueAfterComingBack() {
        assertEquals(0, keepAliveWait(now = 1_000 + KEEP_ALIVE_MS, last = 1_000))
        assertEquals(0, keepAliveWait(now = 1_000 + 20 * 60_000L, last = 1_000))
    }
}
