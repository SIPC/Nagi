package ink.sipc.nagi.data

import ink.sipc.nagi.proto.v1.AppInfoResponse
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class AccessTest {
    private fun info(role: String, roleId: Long = 0, builtin: String = "", vararg perms: String) =
        AppInfoResponse.newBuilder().setRole(role).setRoleId(roleId).setRoleBuiltin(builtin).addAllPermissions(perms.toList()).build()

    @Test
    fun adminCanEverything() {
        val a = Access.of(info("admin", 1, "admin"))
        assertTrue(a.isAdmin)
        assertTrue(a.can(Access.PANEL_SECURITY))
        assertTrue(a.can("anything"))
    }

    @Test
    fun customRoleUsesPanelPermissions() {
        val a = Access.of(info("viewer", 7, "", Access.PROJECT_VIEW, Access.PANEL_MEMBERS))
        assertFalse(a.isAdmin)
        assertTrue(a.can(Access.PANEL_MEMBERS))
        assertTrue(a.can(Access.PROJECT_VIEW))
        assertFalse(a.can(Access.NODE_VIEW))
        assertFalse(a.can(Access.PROJECT_DEPLOY))
        assertEquals("", a.roleBuiltin)
    }

    // 角色一项权限都没有：不能当成旧版面板的只读。
    @Test
    fun emptyRoleCanNothing() {
        val a = Access.of(info("viewer", 9))
        assertFalse(a.can(Access.PROJECT_VIEW))
        assertFalse(a.can(Access.NODE_VIEW))
    }

    // 旧版面板不返回权限和角色 ID：按以前的三种角色算。
    @Test
    fun legacyPanel() {
        val op = Access.of(info("operator"))
        assertTrue(op.can(Access.PROJECT_DEPLOY))
        assertTrue(op.can(Access.NODE_VIEW))
        assertFalse(op.can(Access.NODE_MANAGE))
        assertFalse(op.can(Access.PANEL_MEMBERS))
        assertEquals("operator", op.roleBuiltin)
        val viewer = Access.of(info("viewer"))
        assertTrue(viewer.can(Access.PROJECT_VIEW))
        assertFalse(viewer.can(Access.PROJECT_DEPLOY))
        assertTrue(Access.of(info("admin")).can(Access.PANEL_SETTINGS))
    }
}
