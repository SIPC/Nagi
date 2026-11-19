package ink.sipc.nagi.data

import ink.sipc.nagi.proto.v1.AppInfoResponse

/**
 * 当前成员的角色和权限（见 docs/panel.md#权限），来自 AppService.Info。App 按它隐藏入口，和网页一样；
 * 规则以面板为准，接口照样会拒绝。权限名称和 internal/server/auth/perms.go 一致。
 */
class Access(
    /** 给旧版本看的角色名：admin、operator、viewer。 */
    val role: String,
    private val permissions: Set<String>,
    /** 角色名称，预置角色没有改名时为空。 */
    val roleName: String = "",
    /** admin、operator、viewer，自定义角色为空。 */
    val roleBuiltin: String = "",
    /**
     * 添加和修改成员、添加备用面板、提升为主面板在这个面板上只认密码或验证码做的高权限确认（v0.35.0 起），手机上做不了：
     * 入口做成只读或隐藏，写明在网页上操作。更早的面板照常能做。
     */
    val webOnlyChanges: Boolean = false,
) {
    val isAdmin: Boolean get() = role == "admin"

    fun can(permission: String): Boolean = isAdmin || permission in permissions

    companion object {
        const val PROJECT_VIEW = "project.view"
        const val PROJECT_DEPLOY = "project.deploy"
        const val PROJECT_ADDRESSES = "project.addresses"
        const val NODE_VIEW = "node.view"
        const val NODE_MANAGE = "node.manage"
        const val NODE_DETAILS = "node.details"
        const val NODE_IMAGES = "node.images"
        const val NODE_ROOT = "node.root"
        const val PANEL_MEMBERS = "panel.members"
        const val PANEL_SETTINGS = "panel.settings"
        const val PANEL_SECURITY = "panel.security"

        // 旧版面板不返回权限（role_id 为 0）：按以前三种角色能做的事算。
        private val legacyViewer = setOf(PROJECT_VIEW, "project.logs", NODE_VIEW)
        private val legacyOperator = legacyViewer + setOf(
            "project.config", "project.control", PROJECT_DEPLOY, "project.terminal", "project.files.read", "project.files.write",
            "project.snapshots", "project.backup", "project.ha", "project.migrate", "project.network", PROJECT_ADDRESSES,
            "project.alerts", "project.registries", "project.manage",
        )

        fun of(info: AppInfoResponse): Access {
            val legacy = info.roleId == 0L && info.permissionsCount == 0
            val perms = when {
                !legacy -> info.permissionsList.toSet()
                info.role == "operator" -> legacyOperator
                else -> legacyViewer
            }
            return Access(info.role, perms, info.roleName, info.roleBuiltin.ifEmpty { if (legacy) info.role else "" }, passwordOnlyChanges(info.version))
        }

        /**
         * 按面板的版本号判断（功能列表里没有对应的项）：v0.35.0 及以后为 true。认不出的版本号（开发构建的 dev、ci）
         * 按当前代码算；没有版本号时按旧面板算，操作照常显示，面板拒绝时再提示。
         */
        internal fun passwordOnlyChanges(version: String): Boolean {
            if (version.isEmpty()) return false
            val m = Regex("""^v?(\d+)\.(\d+)""").find(version) ?: return true
            val major = m.groupValues[1].toIntOrNull() ?: return true
            val minor = m.groupValues[2].toIntOrNull() ?: return true
            return major > 0 || minor >= 35
        }
    }
}
