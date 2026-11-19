package ink.sipc.nagi.settings

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
import androidx.compose.runtime.LaunchedEffect
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

// 成员（管理员，和网页 pages/members.tsx 一样）：列表，点一个成员进去改角色和访问范围、重置密码、清除两步验证和通行密钥、删除；
// 右上角加成员。访问范围在手机上按项目、节点逐个勾选；网页上按分组、标签选的照样保留，在这里不改动它们。

private val roles = listOf("admin", "operator", "viewer")

@Composable
private fun roleName(role: String) = stringResource(
    when (role) {
        "admin" -> R.string.role_admin
        "operator" -> R.string.role_operator
        else -> R.string.role_viewer
    },
)

@Composable
private fun roleHint(role: String) = stringResource(
    when (role) {
        "admin" -> R.string.role_admin_hint
        "operator" -> R.string.role_operator_hint
        else -> R.string.role_viewer_hint
    },
)

/** 访问范围的一句话：全部项目 · 3 个节点。分组和标签也写上。 */
@Composable
private fun scopeText(u: PanelUser): String {
    if (u.role == "admin") return stringResource(R.string.scope_everything)
    val s = u.scope
    val projects = if (s.allProjects) stringResource(R.string.scope_all_projects)
    else (s.projectGroupsList.map { stringResource(R.string.scope_group, it) } + stringResource(R.string.scope_project_count, s.projectIdsCount)).joinToString("、")
    val nodes = if (s.allNodes) stringResource(R.string.scope_all_nodes)
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
                            listOfNotNull(u.username.takeIf { u.displayName.isNotEmpty() }, roleName(u.role), scopeText(u).takeIf { u.role != "admin" }).joinToString(" · "),
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
    val action = rememberAction()
    var dialog by remember { mutableStateOf<String?>(null) }
    // 打开、关上确认框时清掉上一次的错误，免得别的操作的错误写进这个确认框。
    LaunchedEffect(dialog) { action.error = null }
    val u = users.value?.firstOrNull { it.id == id }
    SubPage(u?.let { it.displayName.ifEmpty { it.username } } ?: stringResource(R.string.members), onBack) {
        ActionError(action)
        Loaded(users) { list ->
            val user = list.firstOrNull { it.id == id } ?: return@Loaded Note(stringResource(R.string.member_gone))
            var role by remember(user) { mutableStateOf(user.role) }
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
                RolePick(role) { role = it }
            }
            if (role != "admin") ScopeFields(session, scope) { scope = it }
            Spacer(Modifier.height(16.dp))
            val changed = role != user.role || (role != "admin" && scope != (user.scope.takeIf { user.hasScope() } ?: fullScope))
            NagiButton(stringResource(R.string.save), {
                if (user.isSelf && user.role == "admin" && role != "admin") {
                    dialog = "demote"
                } else {
                    action.run("save") { saveAccess(session, user, role, scope); users.reload() }
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
                    title = stringResource(R.string.member_demote_self), text = stringResource(R.string.member_demote_self_confirm, roleName(role)),
                    confirm = stringResource(R.string.member_demote_self), cancel = stringResource(R.string.cancel), danger = true, busy = action.busy == "save",
                    onDismiss = { dialog = null },
                    onConfirm = { action.run("save") { saveAccess(session, user, role, scope); dialog = null; users.reload() } },
                ) { ActionError(action) }
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
                ) { ActionError(action) }
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
                ) { ActionError(action) }
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
                ) { ActionError(action) }
            }
        }
    }
}

private val fullScope: UserScope = UserScope.newBuilder().setAllProjects(true).setAllNodes(true).build()

private suspend fun saveAccess(session: PanelSession, user: PanelUser, role: String, scope: UserScope) {
    val req = UpdateUserRequest.newBuilder().setId(user.id).setRole(if (role == user.role) "" else role)
    if (role != "admin") req.setScope(scope)
    session.rpc { c, h -> c.settings.updateUser(req.build(), h) }
}

/** 加成员：用户名、初始密码、角色、访问范围。 */
@Composable
fun MemberAddScreen(session: PanelSession, onBack: () -> Unit) {
    var username by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var role by remember { mutableStateOf("operator") }
    var scope by remember { mutableStateOf(fullScope) }
    val action = rememberAction()
    SubPage(stringResource(R.string.member_add), onBack) {
        Spacer(Modifier.height(Dimens.topBarToContent + 8.dp))
        Label(stringResource(R.string.member_username))
        NagiTextField(username, { username = it }, label = stringResource(R.string.member_username), maxLength = 32)
        Spacer(Modifier.height(12.dp))
        Label(stringResource(R.string.member_password))
        NagiTextField(password, { password = it }, label = stringResource(R.string.member_password), placeholder = stringResource(R.string.password_hint), password = true)
        SettingsGroup(stringResource(R.string.member_role)) { RolePick(role) { role = it } }
        if (role != "admin") ScopeFields(session, scope) { scope = it }
        ActionError(action)
        Spacer(Modifier.height(16.dp))
        NagiButton(stringResource(R.string.member_add), {
            action.run("add") {
                session.rpc { c, h ->
                    c.settings.createUser(
                        CreateUserRequest.newBuilder().setUsername(username.trim()).setPassword(password).setRole(role).setScope(if (role == "admin") fullScope else scope).build(), h,
                    )
                }
                onBack()
            }
        }, Modifier.fillMaxWidth(), enabled = username.isNotBlank() && password.length >= 8, loading = action.busy == "add")
    }
}

@Composable
private fun Label(text: String) {
    Text(text, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground, modifier = Modifier.height(22.dp))
}

/** 角色：三项单选，每项下面写能做什么。 */
@Composable
private fun RolePick(role: String, onChange: (String) -> Unit) {
    roles.forEach { r ->
        CheckRow(roleName(r), roleHint(r), r == role, radio = true) { onChange(r) }
    }
}

/**
 * 访问范围：项目、节点各一个「全部 / 指定」，指定时列出可以勾选的项。网页上按分组、标签选的保留着，写在列表上面。
 */
@Composable
private fun ScopeFields(session: PanelSession, scope: UserScope, onChange: (UserScope) -> Unit) {
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
        UnderlineTabs(listOf(all, some), if (scope.allNodes) 0 else 1, { onChange(scope.toBuilder().setAllNodes(it == 0).build()) })
        if (!scope.allNodes) {
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
        Note(stringResource(R.string.scope_nodes_hint))
    }
}

/** 勾选的一行：左边方框（[radio] 时是圆圈），名称和灰色说明。 */
@Composable
internal fun CheckRow(title: String, description: String?, checked: Boolean, radio: Boolean = false, onClick: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().rowClickable(onClick, role = if (radio) Role.RadioButton else Role.Checkbox).heightIn(min = Dimens.rowHeight),
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
