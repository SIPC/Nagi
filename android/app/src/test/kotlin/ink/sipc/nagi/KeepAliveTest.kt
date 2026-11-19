package ink.sipc.nagi

import org.junit.Assert.assertEquals
import org.junit.Test

// 前台换令牌的间隔：按上一次真正换到令牌的时间算，离开前台的时间也算在内。
class KeepAliveTest {
    @Test
    fun waitsTheRestOfTheInterval() {
        assertEquals(KEEP_ALIVE_MS, keepAliveWait(sinceMs = 0))
        assertEquals(KEEP_ALIVE_MS - 90_000, keepAliveWait(sinceMs = 90_000))
    }

    @Test
    fun overdueAfterComingBack() {
        assertEquals(0, keepAliveWait(sinceMs = KEEP_ALIVE_MS))
        assertEquals(0, keepAliveWait(sinceMs = 20 * 60_000L))
        // 还没换过令牌。
        assertEquals(0, keepAliveWait(sinceMs = Long.MAX_VALUE))
    }

    @Test
    fun clockMovedBack() {
        assertEquals(KEEP_ALIVE_MS, keepAliveWait(sinceMs = -60_000))
    }
}
