package ink.sipc.nagi.ui

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

// 头像按尺寸缩小解码（安全审查 v0.35.0#17）。
class AvatarTest {
    @Test
    fun smallImagesAreNotScaled() {
        assertEquals(1, avatarSampleSize(128, 128))
        assertEquals(1, avatarSampleSize(256, 100))
    }

    @Test
    fun hugeImagesAreScaledBelowTarget() {
        // 审查里的例子：5400×5400 的全透明 PNG，原样解出来约 117 MB。
        val s = avatarSampleSize(5400, 5400)
        assertEquals(32, s)
        assertTrue(5400 / s <= AVATAR_MAX_PX)
        // 很长的图按长边算。
        val long = avatarSampleSize(100_000, 10)
        assertTrue(100_000 / long <= AVATAR_MAX_PX)
        // 解出来的像素不超过 256×256。
        for ((w, h) in listOf(257 to 257, 1024 to 768, 16384 to 16384, Int.MAX_VALUE to Int.MAX_VALUE)) {
            val k = avatarSampleSize(w, h)
            assertTrue("$w×$h → $k", w / k <= AVATAR_MAX_PX && h / k <= AVATAR_MAX_PX)
        }
    }

    @Test
    fun unreadableSizeMeansNoImage() {
        assertEquals(0, avatarSampleSize(-1, -1))
        assertEquals(0, avatarSampleSize(0, 100))
    }
}
