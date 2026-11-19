package ink.sipc.nagi

import android.content.Intent
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

// 外部的配对链接什么时候收（安全审查 v0.34.0-b L4）。
class PairLinkTest {
    @Test
    fun onlyWithoutPanel() {
        assertTrue(acceptsPairLink(0, hasPanel = false))
        assertFalse(acceptsPairLink(0, hasPanel = true))
    }

    @Test
    fun relaunchFromRecentsIsNotANewLink() {
        assertFalse(acceptsPairLink(Intent.FLAG_ACTIVITY_LAUNCHED_FROM_HISTORY, hasPanel = false))
        assertTrue(acceptsPairLink(Intent.FLAG_ACTIVITY_NEW_TASK, hasPanel = false))
    }

    @Test
    fun ignoredWhileScanningOrConfirming() {
        assertTrue(acceptsPairLinkOn(null))
        assertTrue(acceptsPairLinkOn("welcome"))
        assertFalse(acceptsPairLinkOn("scan"))
        assertFalse(acceptsPairLinkOn("confirm"))
    }
}
