package ink.sipc.nagi.settings

import androidx.compose.foundation.ScrollState
import android.app.Activity
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.widget.Toast
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ink.sipc.nagi.data.AppLanguage
import ink.sipc.nagi.data.AppPrefs
import ink.sipc.nagi.data.CrashEntry
import ink.sipc.nagi.data.CrashLog
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.data.ThemeMode
import ink.sipc.nagi.ui.Avatar
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.ChoiceRow
import ink.sipc.nagi.ui.Collapse
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.EmptyState
import ink.sipc.nagi.ui.KeyValueRow
import ink.sipc.nagi.ui.ListRow
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiIconButton
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.rowClickable
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SettingsGroup
import ink.sipc.nagi.ui.SettingsRow
import kotlinx.coroutines.launch
import java.security.MessageDigest
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

// 设置（见 docs/android.md#本机设置）：上面是账号，然后是「本机」一组，下面是面板的设置，和网页的二级菜单一样。
// 面板的设置这一版只有入口，点进去提示在网页上设置。

/** 设置的二级页面（AppRoot 的导航路由）。 */
object SettingsRoutes {
    const val PANEL = "settings/panel"
    const val PROFILE = "settings/profile"
    const val SECURITY = "settings/security"
    const val PASSWORD = "settings/security/password"
    const val ACCESS = "settings/access"
    const val ACCESS_ADDRESS = "settings/access/address"
    const val ACCESS_DOMAIN = "settings/access/domain"
    const val PANEL_SECURITY = "settings/panel-security"
    const val MEMBERS = "settings/members"
    const val MEMBER = "settings/members/{id}"
    const val MEMBER_ADD = "settings/member-add"
    fun member(id: Long) = "settings/members/$id"
    const val ALERTS = "settings/alerts"
    const val CHANNEL = "settings/alerts/channel/{id}"
    fun channel(id: String) = "settings/alerts/channel/$id"
    const val STANDBY = "settings/standby"
    const val INVITE = "settings/invite"
    const val MESH = "settings/mesh"
    const val PANEL_NAME = "settings/panel/name"
    const val PANEL_ALIAS = "settings/panel/alias"
    const val APPEARANCE = "settings/appearance"
    const val LANGUAGE = "settings/language"
    const val ABOUT = "settings/about"
    const val CRASHES = "settings/crashes"
    const val CRASH = "settings/crash/{at}"
    const val LICENSES = "settings/licenses"

    fun crash(at: Long) = "settings/crash/$at"

    /** 面板设置的一项（和网页设置页 tab 参数同名）对应的子页。 */
    fun of(key: String) = when (key) {
        "profile" -> PROFILE
        "security" -> SECURITY
        "access" -> ACCESS
        "panel-security" -> PANEL_SECURITY
        "members" -> MEMBERS
        "alerts" -> ALERTS
        "standby" -> STANDBY
        "invite" -> INVITE
        "mesh" -> MESH
        else -> ABOUT
    }
}

/** 面板设置的各项，和网页设置的二级菜单同样的分组和顺序。不是管理员时只有我的账号和关于。 */
private val accountKeys = listOf("profile", "security")
private val panelKeys = listOf("access", "panel-security", "members", "alerts", "standby")
private val nodeKeys = listOf("invite", "mesh")

@Composable
private fun keyTitle(key: String): String = stringResource(
    when (key) {
        "profile" -> R.string.profile
        "security" -> R.string.account_security
        "access" -> R.string.panel_access
        "panel-security" -> R.string.panel_security
        "members" -> R.string.members
        "alerts" -> R.string.alerts
        "standby" -> R.string.standby
        "invite" -> R.string.invite
        "mesh" -> R.string.mesh
        else -> R.string.about
    },
)

@Composable
private fun roleLabel(role: String): String = when (role) {
    "admin" -> stringResource(R.string.role_admin)
    "operator" -> stringResource(R.string.role_operator)
    "viewer" -> stringResource(R.string.role_viewer)
    else -> role
}

@Composable
private fun themeLabel(mode: ThemeMode) = stringResource(
    when (mode) {
        ThemeMode.System -> R.string.appearance_system
        ThemeMode.Light -> R.string.appearance_light
        ThemeMode.Dark -> R.string.appearance_dark
    },
)

// 语言名称用各自的语言写，和系统的语言列表一样。
@Composable
private fun languageLabel(language: AppLanguage) = when (language) {
    AppLanguage.System -> stringResource(R.string.language_system)
    AppLanguage.Chinese -> "中文"
    AppLanguage.English -> "English"
}

/**
 * 设置（胶囊导航的第四项）：账号，「本机」（面板、外观、语言、关于 App），面板的设置。
 * 名称先用本机记下的，角色、头像和面板版本等 Info 回来再显示；管理员才有的几组等知道角色后展开。
 */
@Composable
fun SettingsContent(
    vm: SettingsViewModel,
    session: PanelSession,
    prefs: AppPrefs,
    appVersion: String,
    contentPadding: PaddingValues,
    scroll: ScrollState = rememberScrollState(),
    selected: String? = null,
    open: (String) -> Unit,
) {
    // 宽屏时右边正在显示的子页，左边对应的一行有底色。
    fun on(route: String) = selected == route || selected?.startsWith("$route/") == true
    val info by vm.info.collectAsStateWithLifecycle()
    val theme by prefs.theme.collectAsStateWithLifecycle()
    val panel by session.current.collectAsStateWithLifecycle()
    val context = LocalContext.current
    val i = info
    val name = i?.displayName?.ifEmpty { null } ?: i?.username ?: session.panel.displayName.ifEmpty { session.panel.username }
    Column(Modifier.fillMaxSize().verticalScroll(scroll).padding(contentPadding).padding(horizontal = Dimens.pageHorizontal)) {
        Spacer(Modifier.height(Dimens.topBarToContent))
        // 账号：头像、名称、角色。点了去个人资料。
        Row(
            Modifier.fillMaxWidth().rowClickable({ open(SettingsRoutes.PROFILE) }, highlight = if (on(SettingsRoutes.PROFILE)) NagiTheme.colors.muted else null).heightIn(min = 64.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Avatar(name, i?.avatar?.toByteArray(), size = 48.dp)
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f)) {
                Text(name, style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Collapse(i != null) {
                    Text(
                        listOfNotNull(i?.role?.let { roleLabel(it) }, i?.username?.takeIf { it != name }).joinToString(" · "),
                        style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground,
                    )
                }
            }
        }
        SettingsGroup(stringResource(R.string.local)) {
            SettingsRow(stringResource(R.string.panel), { open(SettingsRoutes.PANEL) }, value = panel.title, selected = on(SettingsRoutes.PANEL))
            SettingsRow(stringResource(R.string.appearance), { open(SettingsRoutes.APPEARANCE) }, value = themeLabel(theme), selected = on(SettingsRoutes.APPEARANCE))
            SettingsRow(stringResource(R.string.language), { open(SettingsRoutes.LANGUAGE) }, value = languageLabel(AppLocales.current(context, prefs)), selected = on(SettingsRoutes.LANGUAGE))
        }
        SettingsGroup(stringResource(R.string.group_account)) {
            accountKeys.forEach { SettingsRow(keyTitle(it), { open(SettingsRoutes.of(it)) }, selected = on(SettingsRoutes.of(it))) }
        }
        Collapse(i?.role == "admin") {
            SettingsGroup(stringResource(R.string.group_panel)) {
                panelKeys.forEach { SettingsRow(keyTitle(it), { open(SettingsRoutes.of(it)) }, selected = on(SettingsRoutes.of(it))) }
            }
            SettingsGroup(stringResource(R.string.group_nodes)) {
                nodeKeys.forEach { SettingsRow(keyTitle(it), { open(SettingsRoutes.of(it)) }, selected = on(SettingsRoutes.of(it))) }
            }
        }
        // 关于只有一项，放在最下面，不写组标题（和网页一样）：App 和面板的版本都在里面。
        SettingsGroup(null) {
            SettingsRow(stringResource(R.string.about), { open(SettingsRoutes.ABOUT) }, selected = on(SettingsRoutes.ABOUT))
        }
    }
}

/** 二级页面：返回、标题，下面是可以滚动的内容，左右 20。 */
@Composable
internal fun SubPage(title: String, onBack: () -> Unit, actions: @Composable androidx.compose.foundation.layout.RowScope.() -> Unit = {}, content: @Composable ColumnScope.() -> Unit) {
    Column(Modifier.fillMaxSize().navigationBarsPadding()) {
        NagiBackTopBar(title, onBack = onBack, actions = actions)
        Column(
            Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = Dimens.pageHorizontal).padding(bottom = 24.dp),
            content = content,
        )
    }
}

/** 外观：跟随系统、浅色、深色，选了马上生效。 */
@Composable
fun AppearanceScreen(prefs: AppPrefs, onBack: () -> Unit) {
    val theme by prefs.theme.collectAsStateWithLifecycle()
    SubPage(stringResource(R.string.appearance), onBack) {
        ThemeMode.entries.forEach { m -> ChoiceRow(themeLabel(m), m == theme, { prefs.setTheme(m) }) }
    }
}

/** 语言：跟随系统、中文、English，选了马上切换（界面重建，停在这一页）。 */
@Composable
fun LanguageScreen(prefs: AppPrefs, onBack: () -> Unit) {
    val context = LocalContext.current
    val current = AppLocales.current(context, prefs)
    SubPage(stringResource(R.string.language), onBack) {
        AppLanguage.entries.forEach { l ->
            ChoiceRow(languageLabel(l), l == current, {
                if (l != current) (context as? Activity)?.let { AppLocales.apply(it, prefs, l) }
            })
        }
    }
}

/**
 * 当前面板：名称、地址、域名、成员，和退出这台手机（先确认）。顶部栏的面板列表也用它。
 * 起了本机显示名称时，标题是显示名称，下面另写面板名称。[extra] 放在退出按钮上面（设置里的改名入口）。
 */
@Composable
fun PanelDetails(session: PanelSession, onLogout: () -> Unit, modifier: Modifier = Modifier, extra: @Composable ColumnScope.() -> Unit = {}) {
    var confirming by remember { mutableStateOf(false) }
    val p by session.current.collectAsStateWithLifecycle()
    Column(modifier.fillMaxWidth()) {
        Text(p.title, style = NagiTheme.type.section, color = NagiTheme.colors.foreground)
        Spacer(Modifier.height(8.dp))
        if (p.alias.isNotBlank()) KeyValueRow(stringResource(R.string.panel_name), p.name, labelWidth = 72.dp)
        KeyValueRow(stringResource(R.string.panel_member), p.displayName.ifEmpty { p.username }, labelWidth = 72.dp)
        KeyValueRow(stringResource(R.string.panel_address), p.address, mono = true, labelWidth = 72.dp)
        if (p.domain.isNotEmpty()) KeyValueRow(stringResource(R.string.panel_domain), p.domain, mono = true, labelWidth = 72.dp)
        extra()
        Spacer(Modifier.height(24.dp))
        if (!confirming) {
            NagiButton(stringResource(R.string.panel_logout), { confirming = true }, Modifier.fillMaxWidth(), variant = ButtonVariant.Outline)
        } else {
            Text(stringResource(R.string.panel_logout_confirm), style = NagiTheme.type.body, color = NagiTheme.colors.foreground)
            Spacer(Modifier.height(16.dp))
            NagiButton(stringResource(R.string.sign_out), onLogout, Modifier.fillMaxWidth(), variant = ButtonVariant.Destructive)
            Spacer(Modifier.height(12.dp))
            NagiButton(stringResource(R.string.cancel), { confirming = false }, Modifier.fillMaxWidth(), variant = ButtonVariant.Outline)
        }
    }
}

/** 设置 → 面板：面板的信息，本机显示名称，管理员还能改面板名称。 */
@Composable
fun PanelScreen(session: PanelSession, isAdmin: Boolean, onBack: () -> Unit, onLogout: () -> Unit, open: (String) -> Unit) {
    val p by session.current.collectAsStateWithLifecycle()
    val notSet = stringResource(R.string.not_set)
    SubPage(stringResource(R.string.panel), onBack) {
        Spacer(Modifier.height(Dimens.topBarToContent))
        PanelDetails(session, onLogout) {
            SettingsGroup(null) {
                SettingsRow(stringResource(R.string.panel_alias), { open(SettingsRoutes.PANEL_ALIAS) }, value = p.alias.ifBlank { notSet })
                if (isAdmin) SettingsRow(stringResource(R.string.panel_name), { open(SettingsRoutes.PANEL_NAME) }, value = p.name)
            }
        }
    }
}

/** 改本机显示名称：只存在这台手机上，马上生效。 */
@Composable
fun PanelAliasScreen(session: PanelSession, onBack: () -> Unit) {
    val p by session.current.collectAsStateWithLifecycle()
    EditNameScreen(
        title = stringResource(R.string.panel_alias), initial = p.alias, placeholder = p.name,
        help = stringResource(R.string.panel_alias_help), onBack = onBack,
    ) { session.setAlias(it) }
}

/** 改面板名称（管理员）：所有成员看到的都变，留空恢复成主机名。 */
@Composable
fun PanelNameScreen(session: PanelSession, defaultName: String?, onBack: () -> Unit) {
    val p by session.current.collectAsStateWithLifecycle()
    EditNameScreen(
        title = stringResource(R.string.panel_name), initial = p.name, placeholder = defaultName.orEmpty(),
        help = stringResource(R.string.panel_name_help, defaultName.orEmpty()), onBack = onBack,
    ) { session.setPanelName(it) }
}

/** 改一项文字（名称、地址、域名）的整页：顶部栏和 [EditField]。 */
@Composable
internal fun EditNameScreen(
    title: String,
    initial: String,
    placeholder: String,
    help: String,
    onBack: () -> Unit,
    maxLength: Int = 64,
    saveLabel: String? = null,
    save: suspend (String) -> Unit,
) {
    SubPage(title, onBack) {
        EditField(initial, placeholder, help, onDone = onBack, label = title, maxLength = maxLength, saveLabel = saveLabel, save = save)
    }
}

/**
 * 关于：上面是 App（版本、签名证书指纹，点一下复制；崩溃日志、开源许可），下面是面板（版本）。
 * [panelVersion] 来自 AppService.Info，还没拿到时不写。
 */
@Composable
fun AboutScreen(appVersion: String, panelVersion: String?, session: PanelSession, isAdmin: Boolean, onBack: () -> Unit, open: (String) -> Unit) {
    val context = LocalContext.current
    val haptic = LocalHapticFeedback.current
    val cert = remember { signingCertSha256(context) }
    val copied = stringResource(R.string.copied)
    SubPage(stringResource(R.string.about), onBack) {
        Text(
            stringResource(R.string.about_group_app), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground,
            modifier = Modifier.padding(top = 8.dp, bottom = 4.dp),
        )
        KeyValueRow(stringResource(R.string.version), appVersion, labelWidth = 72.dp)
        if (cert != null) {
            Column(
                Modifier.fillMaxWidth().rowClickable({
                    copy(context, cert)
                    haptic.performHapticFeedback(HapticFeedbackType.LongPress)
                    // Android 13 起系统自己会提示已复制。
                    if (Build.VERSION.SDK_INT < 33) Toast.makeText(context, copied, Toast.LENGTH_SHORT).show()
                }).padding(vertical = 6.dp),
            ) {
                Text(stringResource(R.string.signing_cert), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
                Spacer(Modifier.height(2.dp))
                Text(cert, style = NagiTheme.type.mono, color = NagiTheme.colors.foreground)
                Spacer(Modifier.height(2.dp))
                Text(stringResource(R.string.signing_cert_note), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            }
        }
        SettingsGroup(null) {
            SettingsRow(stringResource(R.string.crash_logs), { open(SettingsRoutes.CRASHES) })
            SettingsRow(stringResource(R.string.licenses), { open(SettingsRoutes.LICENSES) })
        }
        SettingsGroup(stringResource(R.string.about_group_panel)) {
            KeyValueRow(stringResource(R.string.version), panelVersion?.ifEmpty { null } ?: "—", labelWidth = 72.dp)
            // 检查更新、更新面板只有管理员能做。
            if (isAdmin) PanelUpdateSection(session)
        }
    }
}

/** 崩溃日志：最近的几次，点开看全文和分享。 */
@Composable
fun CrashLogsScreen(log: CrashLog, onBack: () -> Unit, open: (String) -> Unit) {
    var entries by remember { mutableStateOf(log.list()) }
    SubPage(
        stringResource(R.string.crash_logs), onBack,
        actions = {
            if (entries.isNotEmpty()) {
                NagiButton(stringResource(R.string.crash_clear), { log.clear(); entries = log.list() }, variant = ButtonVariant.Ghost)
            }
        },
    ) {
        Text(
            stringResource(R.string.crash_note, CrashLog.KEEP), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground,
            modifier = Modifier.padding(vertical = 8.dp),
        )
        if (entries.isEmpty()) {
            EmptyState(stringResource(R.string.crash_none), Modifier.heightIn(min = 160.dp))
        } else {
            entries.forEach { e -> ListRow(e.title.ifEmpty { "—" }, detail = crashTime(e), onClick = { open(SettingsRoutes.crash(e.at)) }) }
        }
    }
}

/** 一次崩溃的全文（等宽），右上角分享。 */
@Composable
fun CrashLogScreen(log: CrashLog, at: Long, onBack: () -> Unit) {
    val context = LocalContext.current
    val entry = remember(at) { log.list().firstOrNull { it.at == at } }
    val text = remember(entry) { entry?.let(log::read).orEmpty() }
    SubPage(
        entry?.let { crashTime(it) } ?: stringResource(R.string.crash_logs), onBack,
        actions = {
            if (text.isNotEmpty()) {
                NagiIconButton(NagiIcons.Share, stringResource(R.string.crash_share), {
                    val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text)
                    context.startActivity(Intent.createChooser(send, null))
                })
            }
        },
    ) {
        Text(text, style = NagiTheme.type.mono.copy(fontSize = NagiTheme.type.small.fontSize), color = NagiTheme.colors.foreground)
    }
}

private fun crashTime(e: CrashEntry): String = SimpleDateFormat("yyyy-MM-dd HH:mm:ss", Locale.getDefault()).format(Date(e.at))

/** 开源许可：用到的第三方组件和它们的许可证。 */
@Composable
fun LicensesScreen(onBack: () -> Unit) {
    SubPage(stringResource(R.string.licenses), onBack) {
        licenses.forEach { (name, license) -> ListRow(name, detail = license) }
    }
}

private val licenses = listOf(
    "MiSans" to "MiSans Font License (Xiaomi)",
    "AndroidX · Jetpack Compose · CameraX · Biometric" to "Apache License 2.0",
    "Kotlin · kotlinx.coroutines" to "Apache License 2.0",
    "OkHttp · Okio" to "Apache License 2.0",
    "Connect Kotlin" to "Apache License 2.0",
    "Protocol Buffers" to "BSD 3-Clause License",
    "ZXing" to "Apache License 2.0",
)

/** 安装包签名证书的 SHA-256（小写十六进制，不分隔），和发版说明里的写法一样（apksigner --print-certs）。 */
private fun signingCertSha256(context: Context): String? = runCatching {
    val info = context.packageManager.getPackageInfo(context.packageName, PackageManager.GET_SIGNING_CERTIFICATES)
    val cert = info.signingInfo?.apkContentsSigners?.firstOrNull() ?: return null
    MessageDigest.getInstance("SHA-256").digest(cert.toByteArray()).joinToString("") { "%02x".format(it) }
}.getOrNull()

internal fun copy(context: Context, text: String) {
    context.getSystemService(ClipboardManager::class.java)?.setPrimaryClip(ClipData.newPlainText(null, text))
}
