package ink.sipc.nagi.settings

import ink.sipc.nagi.proto.v1.ListRolesRequest
import ink.sipc.nagi.network.PanelException
import com.connectrpc.Code
import androidx.compose.ui.draw.alpha
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.CreateUserRequest
import ink.sipc.nagi.proto.v1.DeleteUserRequest
import ink.sipc.nagi.proto.v1.ListNodesRequest
import ink.sipc.nagi.proto.v1.ListProjectsRequest
import ink.sipc.nagi.proto.v1.ListUsersRequest
import ink.sipc.nagi.proto.v1.PanelUser
import ink.sipc.nagi.proto.v1.UpdateUserRequest
import ink.sipc.nagi.proto.v1.UserScope
import ink.sipc.nagi.ui.ActionRow
import ink.sipc.nagi.ui.Avatar
import ink.sipc.nagi.ui.ButtonSize
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiDialog
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiIconButton
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SettingsGroup
import ink.sipc.nagi.ui.UnderlineTabs
import ink.sipc.nagi.ui.rowClickable

// 成员（要「成员」权限，和网页 pages/members.tsx 一样）：列表，点一个成员进去改角色和访问范围、重置密码、清除两步验证和通行密钥、删除；
// 右上角加成员。角色从面板的角色列表里选（见 docs/panel.md#成员和角色），新建和编辑角色只在网页上做。
// 访问范围在手机上按项目、节点逐个勾选；网页上按分组、标签选的照样保留，在这里不改动它们。

/**
 * 可以选的一个角色。新版面板按 [id] 选；旧版面板没有角色列表（[legacy] 为 true），只有 admin、operator、viewer 三种，按 [builtin] 选。
 * [allowed]：当前成员能不能授予它（不是管理员时只能授予自己有的权限，不能选管理员）。
 */
private class RoleChoice(val id: Long, val name: String, val builtin: String, val permissions: Int, val allowed: Boolean, val legacy: Boolean = false) {
    val isAdmin: Boolean get() = builtin == "admin"
}

private val legacyRoles = listOf("admin", "operator", "viewer").mapIndexed { i, r -> RoleChoice(-1L - i, "", r, 0, allowed = true, legacy = true) }

/** 角色列表。面板没有这个接口（旧版）时用以前的三种角色。 */
private suspend fun loadRoles(session: PanelSession): List<RoleChoice> {
    val res = try {
        session.rpc { c, h -> c.settings.listRoles(ListRolesRequest.getDefaultInstance(), h) }
    } catch (e: PanelException) {
        if (e.code == Code.UNIMPLEMENTED) return legacyRoles
        throw e
    }
    val admin = session.access().isAdmin
    val grantable = res.grantableList.toSet()
    return res.rolesList.map { r ->
        RoleChoice(r.id, r.name, r.builtin, r.permissionsCount, allowed = if (r.builtin == "admin") admin else admin || grantable.containsAll(r.permissionsList))
    }
}

/** 成员现在的角色在列表里是哪一个。 */
private fun List<RoleChoice>.of(u: PanelUser): RoleChoice? =
    firstOrNull { if (it.legacy) it.builtin == u.role else it.id == u.roleId }

private val PanelUser.admin: Boolean get() = roleBuiltin == "admin" || (roleId == 0L && role == "admin")

@Composable
private fun roleHint(r: RoleChoice) = when {
    r.legacy -> stringResource(
        when (r.builtin) {
            "admin" -> R.string.role_admin_hint
            "operator" -> R.string.role_operator_hint
            else -> R.string.role_viewer_hint
        },
    )
    !r.allowed -> stringResource(R.string.role_not_grantable)
    r.isAdmin -> stringResource(R.string.role_all_permissions)
    else -> stringResource(R.string.role_permission_count, r.permissions)
}

/** 访问范围的一句话：全部项目 · 3 个节点。分组和标签也写上。 */
@Composable
private fun scopeText(u: PanelUser): String {
    if (u.admin) return stringResource(R.string.scope_everything)
    val s = u.scope
    val projects = if (s.allProjects) stringResource(R.string.scope_all_projects)
    else (s.projectGroupsList.map { stringResource(R.string.scope_group, it) } + stringResource(R.string.scope_project_count, s.projectIdsCount)).joinToString("、")
    val nodes = if (s.nodesFollow) stringResource(R.string.scope_nodes_follow)
    else if (s.allNodes) stringResource(R.string.scope_all_nodes)
    else (s.nodeTagsList.map { stringResource(R.string.scope_tag, it) } + stringResource(R.string.scope_node_count, s.nodeIdsCount)).joinToString("、")
    return "$projects · $nodes"
}

@Composable
fun MembersScreen(session: PanelSession, onBack: () -> Unit, open: (String) -> Unit) {
    val users = rememberLoad { session.rpc { c, h -> c.settings.listUsers(ListUsersRequest.getDefaultInstance(), h) }.usersList }
    SubPage(stringResource(R.string.members), onBack, actions = {
        NagiIconButton(NagiIcons.Plus, stringResource(R.string.member_add), { open(SettingsRoutes.MEMBER_ADD) })
    }) {
        Loaded(users) { list ->
            if (list.isEmpty()) Note(stringResource(R.string.members_empty))
            list.forEach { u ->
                Row(
                    Modifier.fillMaxWidth().rowClickable({ open(SettingsRoutes.member(u.id)) }).heightIn(min = 60.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Avatar(u.displayName.ifEmpty { u.username }, u.avatar.toByteArray(), size = 32.dp)
                    Spacer(Modifier.width(12.dp))
                    Column(Modifier.weight(1f)) {
                        Text(
                            u.displayName.ifEmpty { u.username } + if (u.isSelf) " · " + stringResource(R.string.member_you) else "",
                            style = NagiTheme.type.body, color = NagiTheme.colors.foreground,
                        )
                        Text(
                            listOfNotNull(u.username.takeIf { u.displayName.isNotEmpty() }, roleLabel(u.roleName, u.roleBuiltin, u.role), scopeText(u).takeIf { !u.admin }).joinToString(" · "),
                            style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground,
                        )
                    }
                    if (u.totpEnabled) Text(stringResource(R.string.member_totp_on), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
                }
            }
        }
    }
}

/** 一个成员：角色和访问范围（保存），重置密码、清除两步验证和通行密钥、删除。 */
@Composable
fun MemberScreen(session: PanelSession, id: Long, onBack: () -> Unit) {
    val users = rememberLoad { session.rpc { c, h -> c.settings.listUsers(ListUsersRequest.getDefaultInstance(), h) }.usersList }
    val roles = rememberLoad { loadRoles(session) }
    val action = rememberAction()
    var dialog by remember { mutableStateOf<String?>(null) }
    val u = users.value?.firstOrNull { it.id == id }
    SubPage(u?.let { it.displayName.ifEmpty { it.username } } ?: stringResource(R.string.members), onBack) {
        ActionError(action)
        Loaded(users) { list ->
            val user = list.firstOrNull { it.id == id } ?: return@Loaded Note(stringResource(R.string.member_gone))
            // 选中的角色（列表里的 id），没动过时是成员现在的角色。
            var pickedId by remember(user) { mutableStateOf<Long?>(null) }
            val current = roles.value?.of(user)
            val picked = roles.value?.firstOrNull { it.id == pickedId } ?: current
            val pickedAdmin = picked?.isAdmin ?: user.admin
            val legacy = roles.value?.firstOrNull()?.legacy == true
            var scope by remember(user) { mutableStateOf(user.scope.takeIf { user.hasScope() } ?: fullScope) }
            Spacer(Modifier.height(Dimens.topBarToContent))
            Row(verticalAlignment = Alignment.CenterVertically) {
                Avatar(user.displayName.ifEmpty { user.username }, user.avatar.toByteArray(), size = 48.dp)
                Spacer(Modifier.width(12.dp))
                Column {
                    Text(user.displayName.ifEmpty { user.username }, style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground)
                    Text(
                        listOfNotNull(user.username, stringResource(R.string.member_totp_on).takeIf { user.totpEnabled }, stringResource(R.string.added_at, date(user.createdAt.seconds))).joinToString(" · "),
                        style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground,
                    )
                }
            }
            SettingsGroup(stringResource(R.string.member_role)) {
                Loaded(roles, rows = 3) { list -> RolePick(list, picked?.id) { pickedId = it } }
            }
            if (!pickedAdmin) ScopeFields(session, scope, legacy) { scope = it }
            Spacer(Modifier.height(16.dp))
            val roleChanged = picked != null && picked.id != current?.id
            val changed = roleChanged || (!pickedAdmin && scope != (user.scope.takeIf { user.hasScope() } ?: fullScope))
            val save: suspend () -> Unit = { saveAccess(session, user, picked.takeIf { roleChanged }, pickedAdmin, scope); users.reload() }
            NagiButton(stringResource(R.string.save), {
                if (user.isSelf && user.admin && !pickedAdmin) {
                    dialog = "demote"
                } else {
                    action.run("save") { save() }
                }
            }, Modifier.fillMaxWidth(), enabled = changed, loading = action.busy == "save")

            SettingsGroup(null) {
                ActionRow(stringResource(R.string.member_reset_password)) {
                    NagiButton(stringResource(R.string.member_reset_password), { dialog = "password" }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                }
                if (user.totpEnabled) {
                    ActionRow(stringResource(R.string.member_clear_totp)) {
                        NagiButton(stringResource(R.string.member_clear), { dialog = "totp" }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    }
                }
                if (user.passkeysCount > 0) {
                    ActionRow(stringResource(R.string.member_clear_passkeys), description = stringResource(R.string.member_passkey_count, user.passkeysCount)) {
                        NagiButton(stringResource(R.string.member_clear), { dialog = "passkeys" }, variant = ButtonVariant.Outline, size = ButtonSize.Small)
                    }
                }
                if (!user.isSelf) {
                    ActionRow(stringResource(R.string.member_delete)) {
                        NagiButton(stringResource(R.string.delete), { dialog = "delete" }, variant = ButtonVariant.Destructive, size = ButtonSize.Small)
                    }
                }
            }

            val name = user.displayName.ifEmpty { user.username }
            when (dialog) {
                "demote" -> NagiDialog(
                    title = stringResource(R.string.member_demote_self), text = stringResource(R.string.member_demote_self_confirm, picked?.let { roleLabel(it.name, it.builtin) }.orEmpty()),
                    confirm = stringResource(R.string.member_demote_self), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "save",
                    onDismiss = { dialog = null },
                    onConfirm = { action.run("save") { save(); dialog = null } },
                )
                "password" -> {
                    var pw by remember { mutableStateOf("") }
                    NagiDialog(
                        title = stringResource(R.string.member_reset_password),
                        text = if (user.passkeysCount > 0) stringResource(R.string.member_reset_clears_passkeys) else null,
                        confirm = stringResource(R.string.member_reset_password), cancel = stringResource(R.string.cancel),
                        busy = action.busy == "password", confirmEnabled = pw.length >= 8,
                        onDismiss = { dialog = null },
                        onConfirm = {
                            action.run("password") {
                                session.rpc { c, h -> c.settings.updateUser(UpdateUserRequest.newBuilder().setId(user.id).setPassword(pw).build(), h) }
                                dialog = null
                                users.reload()
                            }
                        },
                    ) {
                        Spacer(Modifier.height(12.dp))
                        NagiTextField(pw, { pw = it }, label = stringResource(R.string.password_new), placeholder = stringResource(R.string.password_hint), password = true)
                        ActionError(action)
                    }
                }
                "totp" -> NagiDialog(
                    title = stringResource(R.string.member_clear_totp),
                    text = stringResource(if (user.passkeysCount > 0) R.string.member_clear_totp_passkeys_confirm else R.string.member_clear_totp_confirm, name),
                    confirm = stringResource(R.string.member_clear), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "totp",
                    onDismiss = { dialog = null },
                    onConfirm = {
                        action.run("totp") {
                            session.rpc { c, h -> c.settings.updateUser(UpdateUserRequest.newBuilder().setId(user.id).setClearTotp(true).build(), h) }
                            dialog = null
                            users.reload()
                        }
                    },
                )
                "passkeys" -> NagiDialog(
                    title = stringResource(R.string.member_clear_passkeys),
                    text = stringResource(R.string.member_clear_passkeys_confirm, name, user.passkeysList.joinToString("、") { it.name }),
                    confirm = stringResource(R.string.member_clear), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "passkeys",
                    onDismiss = { dialog = null },
                    onConfirm = {
                        action.run("passkeys") {
                            session.rpc { c, h -> c.settings.updateUser(UpdateUserRequest.newBuilder().setId(user.id).setClearPasskeys(true).build(), h) }
                            dialog = null
                            users.reload()
                        }
                    },
                )
                "delete" -> NagiDialog(
                    title = stringResource(R.string.member_delete), text = stringResource(R.string.member_delete_confirm, name),
                    confirm = stringResource(R.string.delete), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "delete",
                    onDismiss = { dialog = null },
                    onConfirm = {
                        action.run("delete") {
                            session.rpc { c, h -> c.settings.deleteUser(DeleteUserRequest.newBuilder().setId(user.id).build(), h) }
                            dialog = null
                            onBack()
                        }
                    },
                )
            }
        }
    }
}

private val fullScope: UserScope = UserScope.newBuilder().setAllProjects(true).setAllNodes(true).build()

/** 新成员默认的范围（和网页一样）：项目全部，节点跟随项目。 */
private val defaultScope: UserScope = UserScope.newBuilder().setAllProjects(true).setNodesFollow(true).build()

/** 保存角色和访问范围。[role] 为 null 表示角色不改；管理员不设范围。 */
private suspend fun saveAccess(session: PanelSession, user: PanelUser, role: RoleChoice?, admin: Boolean, scope: UserScope) {
    val req = UpdateUserRequest.newBuilder().setId(user.id)
    if (role != null) { if (role.legacy) req.setRole(role.builtin) else req.setRoleId(role.id) }
    if (!admin) req.setScope(scope)
    session.rpc { c, h -> c.settings.updateUser(req.build(), h) }
}

/** 加成员：用户名、初始密码、角色、访问范围。 */
@Composable
fun MemberAddScreen(session: PanelSession, onBack: () -> Unit) {
    var username by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    val roles = rememberLoad { loadRoles(session) }
    var pickedId by remember { mutableStateOf<Long?>(null) }
    // 默认选预置的操作员（没有、不能授予时选第一个能授予的、不是管理员的角色）。
    val picked = roles.value?.let { list ->
        list.firstOrNull { it.id == pickedId }
            ?: list.firstOrNull { it.builtin == "operator" && it.allowed }
            ?: list.firstOrNull { !it.isAdmin && it.allowed }
    }
    val legacy = roles.value?.firstOrNull()?.legacy == true
    // 旧版面板不认「跟随项目」，默认全部。
    var edited by remember { mutableStateOf<UserScope?>(null) }
    val scope = edited ?: if (legacy) fullScope else defaultScope
    val action = rememberAction()
    SubPage(stringResource(R.string.member_add), onBack) {
        Spacer(Modifier.height(Dimens.topBarToContent + 8.dp))
        Label(stringResource(R.string.member_username))
        NagiTextField(username, { username = it }, label = stringResource(R.string.member_username), maxLength = 32)
        Spacer(Modifier.height(12.dp))
        Label(stringResource(R.string.member_password))
        NagiTextField(password, { password = it }, label = stringResource(R.string.member_password), placeholder = stringResource(R.string.password_hint), password = true)
        SettingsGroup(stringResource(R.string.member_role)) {
            Loaded(roles, rows = 3) { list -> RolePick(list, picked?.id) { pickedId = it } }
        }
        if (picked?.isAdmin != true) ScopeFields(session, scope, legacy) { edited = it }
        ActionError(action)
        Spacer(Modifier.height(16.dp))
        NagiButton(stringResource(R.string.member_add), {
            val role = picked ?: return@NagiButton
            action.run("add") {
                val req = CreateUserRequest.newBuilder().setUsername(username.trim()).setPassword(password).setScope(if (role.isAdmin) fullScope else scope)
                if (role.legacy) req.setRole(role.builtin) else req.setRoleId(role.id)
                session.rpc { c, h -> c.settings.createUser(req.build(), h) }
                onBack()
            }
        }, Modifier.fillMaxWidth(), enabled = picked != null && username.isNotBlank() && password.length >= 8, loading = action.busy == "add")
    }
}

@Composable
private fun Label(text: String) {
    Text(text, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.height(22.dp))
}

/** 角色：单选，每项下面写有几项权限；不能授予的置灰。新建和编辑角色在网页上做。 */
@Composable
private fun RolePick(roles: List<RoleChoice>, picked: Long?, onChange: (Long) -> Unit) {
    roles.forEach { r ->
        CheckRow(roleLabel(r.name, r.builtin), roleHint(r), r.id == picked, radio = true, enabled = r.allowed || r.id == picked) { onChange(r.id) }
    }
    if (roles.none { it.legacy }) Note(stringResource(R.string.roles_on_web))
}

/**
 * 访问范围：项目「全部 / 指定」，节点「全部 / 跟随项目 / 指定」，指定时列出可以勾选的项。网页上按分组、标签选的保留着，写在列表上面。
 * 跟随项目：只能看到范围内项目的容器所在的节点（见 docs/panel.md#访问范围）；旧版面板（[legacy]）没有这一项。
 */
@Composable
private fun ScopeFields(session: PanelSession, scope: UserScope, legacy: Boolean, onChange: (UserScope) -> Unit) {
    val projects = rememberLoad { session.rpc { c, h -> c.projects.listProjects(ListProjectsRequest.getDefaultInstance(), h) }.projectsList.map { it.project } }
    val nodes = rememberLoad { session.rpc { c, h -> c.nodes.listNodes(ListNodesRequest.getDefaultInstance(), h) }.nodesList.filter { !it.pending } }
    val all = stringResource(R.string.scope_all)
    val some = stringResource(R.string.scope_some)
    SettingsGroup(stringResource(R.string.scope_projects)) {
        UnderlineTabs(listOf(all, some), if (scope.allProjects) 0 else 1, { onChange(scope.toBuilder().setAllProjects(it == 0).build()) })
        if (!scope.allProjects) {
            if (scope.projectGroupsCount > 0) Note(stringResource(R.string.scope_kept, scope.projectGroupsList.joinToString("、")))
            Loaded(projects) { list ->
                if (list.isEmpty()) Note(stringResource(R.string.scope_no_projects))
                list.sortedBy { it.name }.forEach { p ->
                    val on = p.id in scope.projectIdsList
                    CheckRow(p.name, p.group.takeIf { it.isNotEmpty() }, on) {
                        val ids = scope.projectIdsList.toMutableList().apply { if (on) remove(p.id) else add(p.id) }
                        onChange(scope.toBuilder().clearProjectIds().addAllProjectIds(ids).build())
                    }
                }
            }
        }
        Note(stringResource(R.string.scope_projects_hint))
    }
    SettingsGroup(stringResource(R.string.scope_nodes)) {
        if (legacy) {
            UnderlineTabs(listOf(all, some), if (scope.allNodes) 0 else 1, { onChange(scope.toBuilder().setAllNodes(it == 0).build()) })
        } else {
            UnderlineTabs(listOf(all, stringResource(R.string.scope_follow), some), if (scope.allNodes) 0 else if (scope.nodesFollow) 1 else 2, {
                val b = scope.toBuilder().setAllNodes(it == 0).setNodesFollow(it == 1)
                // 跟随项目时不带指定的节点和标签（面板要求）。
                if (it == 1) b.clearNodeIds().clearNodeTags()
                onChange(b.build())
            })
        }
        if (scope.nodesFollow) {
            Note(stringResource(R.string.scope_follow_hint))
        } else if (!scope.allNodes) {
            if (scope.nodeTagsCount > 0) Note(stringResource(R.string.scope_kept, scope.nodeTagsList.joinToString("、")))
            Loaded(nodes) { list ->
                if (list.isEmpty()) Note(stringResource(R.string.scope_no_nodes))
                list.sortedBy { it.name }.forEach { n ->
                    val on = n.id in scope.nodeIdsList
                    CheckRow(n.name, n.tagsList.joinToString("、").takeIf { it.isNotEmpty() }, on) {
                        val ids = scope.nodeIdsList.toMutableList().apply { if (on) remove(n.id) else add(n.id) }
                        onChange(scope.toBuilder().clearNodeIds().addAllNodeIds(ids).build())
                    }
                }
            }
        }
        if (!scope.nodesFollow) Note(stringResource(R.string.scope_nodes_hint))
    }
}

/** 勾选的一行：左边方框（[radio] 时是圆圈），名称和灰色说明。 */
@Composable
internal fun CheckRow(title: String, description: String?, checked: Boolean, radio: Boolean = false, enabled: Boolean = true, onClick: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().rowClickable(onClick, enabled = enabled, role = if (radio) Role.RadioButton else Role.Checkbox).heightIn(min = Dimens.rowHeight)
            .alpha(if (enabled) 1f else 0.5f),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        val c = NagiTheme.colors
        Box(
            Modifier.size(18.dp).border(1.5.dp, if (checked) c.foreground else c.mutedForeground, if (radio) RoundedCornerShape(9.dp) else RoundedCornerShape(4.dp)),
            contentAlignment = Alignment.Center,
        ) {
            if (checked) Icon(NagiIcons.Check, contentDescription = null, tint = c.foreground, modifier = Modifier.size(12.dp))
        }
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(title, style = NagiTheme.type.body, color = c.foreground)
            if (!description.isNullOrEmpty()) Text(description, style = NagiTheme.type.secondary, color = c.mutedForeground)
        }
    }
}
