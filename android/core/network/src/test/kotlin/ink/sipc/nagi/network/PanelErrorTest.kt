package ink.sipc.nagi.network

import com.connectrpc.Code
import com.connectrpc.ConnectException
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

// 面板要求高权限确认时（安全审查 v0.35.0#8：配对备用面板、提升、交接节点、新建和修改成员），
// App 认出来，提示在网页上操作，而不是显示别的拒绝原因。
class PanelErrorTest {
    @Test
    fun elevationHeaderMapsToElevationRequired() {
        val e = ConnectException(Code.PERMISSION_DENIED, message = "请先验证身份", metadata = mapOf("X-Nagi-Elevation" to listOf("required")))
        val p = e.toPanelException()
        assertTrue(p is ElevationRequiredException)
        assertEquals(Code.PERMISSION_DENIED, p.code)
    }

    @Test
    fun plainPermissionDeniedStaysPlain() {
        val e = ConnectException(Code.PERMISSION_DENIED, message = "当前账号没有权限执行这个操作")
        val p = e.toPanelException()
        assertFalse(p is ElevationRequiredException)
        assertEquals("当前账号没有权限执行这个操作", p.message)
    }
}
