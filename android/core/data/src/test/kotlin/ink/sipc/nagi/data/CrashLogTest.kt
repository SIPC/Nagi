package ink.sipc.nagi.data

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test

// 崩溃日志的脱敏（安全审查 2026-11-18-65e6436#2）。
class CrashLogTest {
    @Test
    fun scrubsAddressesDomainsAndPanelText() {
        val raw = """
            java.net.ConnectException: Failed to connect to panel.example.com/203.0.113.5:8443
            Caused by: java.io.IOException: failed to connect to nagi.home.arpa/[2001:db8::1]:443 from /[fe80::1%wlan0]
            Caused by: ink.sipc.nagi.network.PanelException: 项目 billing 的容器 api 不存在
            Caused by: java.lang.IllegalStateException: open nagi-app://pair?v=2&c=abcdefABCDEFabcdefABCD
            	at ink.sipc.nagi.data.PanelSession.overview(PanelSession.kt:99)
            	at ink.sipc.nagi.data.PanelSession${'$'}overview${'$'}1.invokeSuspend(PanelSession.kt:120)
        """.trimIndent()
        val out = CrashLog.scrub(raw)
        for (secret in listOf("panel.example.com", "203.0.113.5", "nagi.home.arpa", "2001:db8::1", "fe80::1", "billing", "abcdefABCDEF")) {
            assertFalse("$secret 还在：$out", out.contains(secret))
        }
        // 调用栈不动，类名、文件名、行号都在。
        assertEquals(raw.lines()[4], out.lines()[4])
        assertEquals(raw.lines()[5], out.lines()[5])
        assertEquals("Caused by: ink.sipc.nagi.network.PanelException: <message>", out.lines()[2])
    }
}
