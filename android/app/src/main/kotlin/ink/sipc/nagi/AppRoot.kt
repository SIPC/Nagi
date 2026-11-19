package ink.sipc.nagi

import ink.sipc.nagi.pairing.ApprovalPrompts
import ink.sipc.nagi.nodes.AddNodeScreen
import ink.sipc.nagi.nodes.NodeActions
import ink.sipc.nagi.nodes.NodeScreen
import ink.sipc.nagi.nodes.NodesContent
import ink.sipc.nagi.nodes.NodesViewModel
import ink.sipc.nagi.nodes.accessOf
import ink.sipc.nagi.data.Polled
import ink.sipc.nagi.ui.NagiIconButton
import ink.sipc.nagi.pairing.rememberElevate
import ink.sipc.nagi.ui.LocalElevate
import androidx.compose.runtime.CompositionLocalProvider
import ink.sipc.nagi.data.Access
import ink.sipc.nagi.ui.Avatar
import androidx.compose.ui.zIndex
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.draw.clip
import androidx.compose.foundation.clickable
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.statusBarsPadding
import ink.sipc.nagi.ui.PillRail
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.NavController
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.Row
import ink.sipc.nagi.settings.MeshScreen
import ink.sipc.nagi.settings.InviteScreen
import ink.sipc.nagi.settings.StandbyScreen
import ink.sipc.nagi.settings.ChannelScreen
import ink.sipc.nagi.settings.AlertsScreen
import ink.sipc.nagi.settings.MemberScreen
import ink.sipc.nagi.settings.MemberAddScreen
import ink.sipc.nagi.settings.MembersScreen
import ink.sipc.nagi.settings.PanelSecurityScreen
import ink.sipc.nagi.settings.AccessDomainScreen
import ink.sipc.nagi.settings.AccessAddressScreen
import ink.sipc.nagi.settings.AccessScreen
import ink.sipc.nagi.settings.PasswordScreen
import ink.sipc.nagi.settings.SecurityScreen
import ink.sipc.nagi.settings.ProfileScreen
import androidx.compose.animation.ExitTransition
import androidx.compose.animation.EnterTransition
import androidx.compose.animation.togetherWith
import androidx.compose.animation.scaleIn
import androidx.navigation.NavGraphBuilder
import androidx.navigation.NavBackStackEntry
import androidx.compose.animation.AnimatedContentTransitionScope
import androidx.compose.animation.AnimatedContentScope
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.asPaddingValues
import androidx.compose.foundation.layout.statusBars
import androidx.compose.foundation.layout.WindowInsets
import ink.sipc.nagi.settings.AboutScreen
import ink.sipc.nagi.settings.AppearanceScreen
import ink.sipc.nagi.settings.CrashLogScreen
import ink.sipc.nagi.settings.CrashLogsScreen
import ink.sipc.nagi.settings.LanguageScreen
import ink.sipc.nagi.settings.LicensesScreen
import ink.sipc.nagi.settings.PanelDetails
import ink.sipc.nagi.settings.PanelScreen
import ink.sipc.nagi.settings.PanelAliasScreen
import ink.sipc.nagi.settings.PanelNameScreen
import ink.sipc.nagi.settings.SettingsContent
import ink.sipc.nagi.settings.SettingsRoutes
import ink.sipc.nagi.settings.SettingsViewModel
import androidx.compose.runtime.setValue
import androidx.compose.runtime.getValue
import ink.sipc.nagi.ui.EmptyState
import ink.sipc.nagi.ui.NagiTransitions
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.slideInVertically
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import ink.sipc.nagi.ui.Motion
import ink.sipc.nagi.ui.isReducedMotion
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import ink.sipc.nagi.data.AppGraph
import ink.sipc.nagi.data.OverviewState
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.home.HomeActions
import ink.sipc.nagi.home.HomeContent
import ink.sipc.nagi.home.HomeViewModel
import ink.sipc.nagi.network.PairingUri
import ink.sipc.nagi.pairing.ConfirmScreen
import ink.sipc.nagi.pairing.PairingViewModel
import ink.sipc.nagi.pairing.ScanScreen
import ink.sipc.nagi.pairing.UnlockScreen
import ink.sipc.nagi.pairing.WelcomeScreen
import ink.sipc.nagi.data.Attention
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiIcons
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.NagiTopBar
import ink.sipc.nagi.ui.PillNav
import ink.sipc.nagi.ui.PillNavItem
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.launch

/** 根：没有面板时是添加流程；有面板时先解锁，再进入四个标签。 */
@Composable
fun AppRoot(graph: AppGraph, lock: AppLock, pendingPair: MutableStateFlow<PairingUri?>) {
    val session by graph.session.collectAsState()
    val locked by lock.locked.collectAsState()
    var removed by rememberSaveable { mutableStateOf(false) }
    // 有面板时不留着外部链接：否则退出这台手机后会自动打开这个旧链接的确认页（v0.34.0-b L4）。
    LaunchedEffect(session) { if (session != null) pendingPair.value = null }
    val reduced = isReducedMotion()
    // 添加流程、解锁、四个标签之间换的是整页：新的一页淡入并从 98% 放大到原大小，旧的一页先淡出，不硬切。
    // 每一页带着自己的会话，淡出的那一页（例如刚退出）不会读到已经没有的会话。
    val root = when {
        session == null -> Root(RootKind.PAIRING, null, null)
        locked != null -> Root(RootKind.LOCKED, session, locked)
        else -> Root(RootKind.MAIN, session, null)
    }
    AnimatedContent(
        targetState = root,
        modifier = Modifier.fillMaxSize().background(NagiTheme.colors.background),
        contentKey = { it.kind to it.session?.panel?.id },
        transitionSpec = {
            if (reduced) {
                fadeIn(tween(Motion.FADE_IN)) togetherWith fadeOut(tween(Motion.ROOT_OUT))
            } else {
                (fadeIn(tween(Motion.ROOT_IN, delayMillis = 60, easing = Motion.Enter)) + scaleIn(tween(Motion.ROOT_IN + 60, easing = Motion.ContentIn), initialScale = 0.98f)) togetherWith
                    fadeOut(tween(Motion.ROOT_OUT, easing = Motion.Exit))
            }
        },
        label = "root",
    ) { r ->
        val s = r.session
        when {
            s == null -> PairingFlow(graph, pendingPair, removed, onPaired = lock::unlocked) {
                removed = false
                lock.unlocked()
            }
            r.kind == RootKind.LOCKED -> UnlockScreen(s, r.locked?.idleMinutes, onUnlocked = lock::unlocked, onRemoved = {
                graph.forget()
                removed = true
            })
            else -> {
                // 高权限确认交给设置里的各个操作用；网页发来的确认请求在主界面上弹出（见 docs/android.md#手机确认）。
                CompositionLocalProvider(LocalElevate provides rememberElevate(s)) {
                    MainTabs(graph, s, onLocked = lock::lockNow, onRemoved = {
                        graph.forget()
                        removed = true
                    })
                }
                ApprovalPrompts(s)
            }
        }
    }
}

private enum class RootKind { PAIRING, LOCKED, MAIN }

private data class Root(val kind: RootKind, val session: PanelSession?, val locked: AppLock.Locked?)

/**
 * 导航里的一页：整页铺上页面底色。进下一级时新的一页从右边滑进来盖住旧的一页，返回时滑出去露出下面的一页；
 * 页面是透明的话，滑动的过程中两页的内容会叠在一起。
 */
private fun NavGraphBuilder.page(
    route: String,
    enterTransition: (AnimatedContentTransitionScope<NavBackStackEntry>.() -> EnterTransition?)? = null,
    exitTransition: (AnimatedContentTransitionScope<NavBackStackEntry>.() -> ExitTransition?)? = null,
    content: @Composable AnimatedContentScope.(NavBackStackEntry) -> Unit,
) = composable(
    route,
    enterTransition = enterTransition,
    exitTransition = exitTransition,
    popEnterTransition = null,
    popExitTransition = null,
) { entry ->
    Box(Modifier.fillMaxSize().background(NagiTheme.colors.background)) { content(entry) }
}

// ---- 添加流程：欢迎 → 扫码 → 确认 ----

private class Factory(private val create: () -> ViewModel) : ViewModelProvider.Factory {
    @Suppress("UNCHECKED_CAST")
    override fun <T : ViewModel> create(modelClass: Class<T>): T = create() as T
}

@Composable
private fun PairingFlow(graph: AppGraph, pendingPair: MutableStateFlow<PairingUri?>, removed: Boolean, onPaired: () -> Unit, onDone: () -> Unit) {
    val nav = rememberNavController()
    val vm: PairingViewModel = viewModel(factory = Factory { PairingViewModel(graph, onPaired) })
    val pending by pendingPair.collectAsState()
    LaunchedEffect(pending) {
        val uri = pending ?: return@LaunchedEffect
        pendingPair.value = null
        // 只在欢迎页接收外部链接：扫码页、确认页打开期间来的不理，别的 App 不能悄悄把确认页换成别的面板（v0.34.0-b L4）。
        if (!acceptsPairLinkOn(nav.currentDestination?.route)) return@LaunchedEffect
        vm.open(uri, external = true)
        nav.navigate("confirm") { launchSingleTop = true }
    }
    val reduced = isReducedMotion()
    val rise = with(LocalDensity.current) { 8.dp.roundToPx() }
    NavHost(
        nav, startDestination = "welcome",
        enterTransition = { NagiTransitions.pushIn(reduced) },
        exitTransition = { NagiTransitions.pushOut(reduced) },
        popEnterTransition = { NagiTransitions.popIn(reduced) },
        popExitTransition = { NagiTransitions.popOut(reduced) },
    ) {
        page("welcome") {
            WelcomeScreen(onScan = { nav.navigate("scan") }, notice = if (removed) stringResource(R.string.removed) else null)
        }
        // 扫码成功时取景框已经展开成整页底色，确认页直接淡入接上，不再从右边滑入；扫码页同时淡出。
        page(
            "scan",
            // 去确认页时扫码页不淡出：它这时已经铺满页面底色，确认页叠在上面淡入的这段时间（240ms）一直完整地垫在下面，
            // 确认页完全显示后才移除。两页同时半透明会透出窗口的背景。
            exitTransition = { if (targetState.destination.route == "confirm") fadeOut(tween(1, delayMillis = 260)) else NagiTransitions.pushOut(reduced) },
        ) {
            ScanScreen(
                onBack = { nav.popBackStack() },
                // 锁定二维码时就开始查配对信息，展开动画期间数据就到了。
                onLocked = { uri -> vm.open(uri) },
                onScanned = { nav.navigate("confirm") { popUpTo("welcome") } },
            )
        }
        page(
            "confirm",
            enterTransition = {
                // 从扫码页来：扫码页已经展开成同样的底色，确认页的内容在上面淡入、轻轻上移 8dp。
                if (initialState.destination.route == "scan") {
                    if (reduced) fadeIn(tween(Motion.FADE_IN))
                    else fadeIn(tween(240, easing = Motion.Enter)) + slideInVertically(tween(320, easing = Motion.ContentIn)) { rise }
                } else {
                    NagiTransitions.pushIn(reduced)
                }
            },
        ) {
            ConfirmScreen(vm, onBack = { nav.popBackStack("welcome", inclusive = false) }, onDone = onDone)
        }
    }
}

// ---- 四个标签 ----

private enum class Tab { HOME, PROJECTS, NODES, SETTINGS }

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MainTabs(graph: AppGraph, session: PanelSession, onLocked: () -> Unit, onRemoved: () -> Unit) {
    val nav = rememberNavController()
    var tab by rememberSaveable { mutableStateOf(Tab.HOME) }
    // 各标签的滚动位置放在这里：切到别的标签再切回来，还停在原来的位置。
    val homeList = rememberLazyListState()
    val settingsScroll = rememberScrollState()
    // 不在首页时按返回先回首页，在首页再按才退出 App（主导航的惯例）。
    BackHandler(enabled = tab != Tab.HOME) { tab = Tab.HOME }
    val snackbar = remember { SnackbarHostState() }
    val scope = rememberCoroutineScope()
    var sheet by remember { mutableStateOf(false) }
    val home: HomeViewModel = viewModel(key = session.panel.id, factory = Factory { HomeViewModel(session) })
    val settings: SettingsViewModel = viewModel(key = "settings-" + session.panel.id, factory = Factory { SettingsViewModel(session) })
    val nodes: NodesViewModel = viewModel(key = "nodes-" + session.panel.id, factory = Factory { NodesViewModel(session) })
    val nodesList = rememberLazyListState()
    // 宽屏时节点页右边正在看的节点（从首页点进来时也用它）；为空时看列表里的第一个。
    var pickedNode by rememberSaveable { mutableStateOf<String?>(null) }
    // 退出这台手机（顶部栏的面板列表、设置里的面板页）：通知面板删除设备记录，再回到欢迎页。
    val logout: () -> Unit = {
        scope.launch {
            runCatching { session.logout() }
            sheet = false
            onRemoved()
        }
    }
    val overview by home.overview.collectAsStateWithLifecycle()
    LaunchedEffect(overview) {
        when (overview) {
            OverviewState.Locked -> onLocked()
            OverviewState.Removed -> onRemoved()
            else -> {}
        }
    }
    val critical = (overview as? OverviewState.Ready)?.overview?.attention?.any { it.severity == "critical" } == true
    val showMessage: (String) -> Unit = { msg -> scope.launch { snackbar.showSnackbar(msg) } }
    // 宽屏（600 dp 以上，平板、横屏、折叠屏展开）：左边竖着的胶囊代替底部的胶囊，一级和二级页面都显示；
    // 设置页、节点页左边是列表，右边是选中的一项（见 docs/android.md#宽屏）。只按窗口宽度判断。
    val wide = LocalConfiguration.current.screenWidthDp >= 600
    // 打开一个节点：手机上推进节点页；宽屏切到节点标签，右边显示这个节点。
    val openNode: (String) -> Unit = { id ->
        if (wide) {
            pickedNode = id
            tab = Tab.NODES
        } else {
            nav.navigate("node/$id")
        }
    }
    val actions = object : HomeActions {
        override fun openAttention(item: Attention) {
            when {
                item.container.isNotEmpty() -> nav.navigate("container/${item.projectId}/${item.container}")
                item.nodeId.isNotEmpty() -> openNode(item.nodeId)
            }
        }

        override fun openNodes(offlineOnly: Boolean) {
            tab = Tab.NODES
        }

        override fun openNode(id: String) = openNode(id)

        override fun openProjects(notRunningOnly: Boolean) {
            tab = Tab.PROJECTS
        }

        override fun update(item: Attention) {
            nav.navigate("container/${item.projectId}/${item.container}")
        }
    }

    val reduced = isReducedMotion()
    val navItems = listOf(
        PillNavItem(NagiIcons.Home, stringResource(R.string.tab_home), badge = critical),
        PillNavItem(NagiIcons.Folder, stringResource(R.string.tab_projects)),
        PillNavItem(NagiIcons.Server, stringResource(R.string.tab_nodes)),
        PillNavItem(NagiIcons.Settings, stringResource(R.string.tab_settings)),
    )
    val current by nav.currentBackStackEntryAsState()
    val selectTab: (Int) -> Unit = { i ->
        // 在二级页面点左边的胶囊：先回到一级页面再切换。
        val onTabs = current?.destination?.route == "tabs"
        if (!onTabs) nav.popBackStack("tabs", inclusive = false)
        if (i == tab.ordinal && onTabs) {
            // 再点一次当前的标签：回到顶部。
            scope.launch {
                when (tab) {
                    Tab.HOME -> if (reduced) homeList.scrollToItem(0) else homeList.animateScrollToItem(0)
                    Tab.NODES -> if (reduced) nodesList.scrollToItem(0) else nodesList.animateScrollToItem(0)
                    Tab.SETTINGS -> if (reduced) settingsScroll.scrollTo(0) else settingsScroll.animateScrollTo(0)
                    else -> {}
                }
            }
        } else {
            tab = Tab.entries[i]
        }
    }

    // 设置的各个子页。窄屏时在主导航里一页一页推进去；宽屏时在设置页右边的那一栏里（[n] 是那一栏自己的导航）。
    fun NavGraphBuilder.settingsPages(n: NavController) {
        val back: () -> Unit = { n.popBackStack() }
        val go: (String) -> Unit = { n.navigate(it) }
        page(SettingsRoutes.PANEL) {
            val info by settings.info.collectAsStateWithLifecycle()
            PanelScreen(session, info?.let { Access.of(it) }, onBack = back, onLogout = logout, open = go)
        }
        page(SettingsRoutes.PANEL_ALIAS) { PanelAliasScreen(session, onBack = back) }
        page(SettingsRoutes.PANEL_NAME) {
            val defaultName by settings.defaultName.collectAsStateWithLifecycle()
            PanelNameScreen(session, defaultName, onBack = back)
        }
        page(SettingsRoutes.APPEARANCE) { AppearanceScreen(graph.prefs, onBack = back) }
        page(SettingsRoutes.LANGUAGE) { LanguageScreen(graph.prefs, onBack = back) }
        page(SettingsRoutes.ABOUT) {
            val info by settings.info.collectAsStateWithLifecycle()
            AboutScreen(BuildConfig.VERSION_NAME, info?.version, session, info?.let { Access.of(it) }, onBack = back, open = go)
        }
        page(SettingsRoutes.PROFILE) { ProfileScreen(session, onBack = back, onSaved = settings::refresh) }
        page(SettingsRoutes.SECURITY) { SecurityScreen(session, onBack = back, open = go, onSignedOut = onRemoved) }
        page(SettingsRoutes.PASSWORD) { PasswordScreen(session, onBack = back, onSignedOut = onRemoved) }
        page(SettingsRoutes.ACCESS) {
            val info by settings.info.collectAsStateWithLifecycle()
            AccessScreen(session, info?.role == "admin", onBack = back, open = go)
        }
        page(SettingsRoutes.ACCESS_ADDRESS) { AccessAddressScreen(session, onBack = back) }
        page(SettingsRoutes.ACCESS_DOMAIN) { AccessDomainScreen(session, onBack = back) }
        page(SettingsRoutes.PANEL_SECURITY) { PanelSecurityScreen(session, onBack = back) }
        page(SettingsRoutes.MEMBERS) { MembersScreen(session, onBack = back, open = go) }
        page(SettingsRoutes.MEMBER_ADD) { MemberAddScreen(session, onBack = back) }
        page(SettingsRoutes.MEMBER) { entry -> MemberScreen(session, entry.arguments?.getString("id")?.toLongOrNull() ?: 0L, onBack = back) }
        page(SettingsRoutes.ALERTS) { AlertsScreen(session, onBack = back, open = go) }
        page(SettingsRoutes.CHANNEL) { entry -> ChannelScreen(session, entry.arguments?.getString("id").orEmpty(), onBack = back) }
        page(SettingsRoutes.STANDBY) { StandbyScreen(session, onBack = back) }
        page(SettingsRoutes.INVITE) { InviteScreen(session, onBack = back) }
        page(SettingsRoutes.MESH) { MeshScreen(session, onBack = back) }
        page(SettingsRoutes.CRASHES) { CrashLogsScreen(graph.crashLog, onBack = back, open = go) }
        page(SettingsRoutes.CRASH) { entry ->
            CrashLogScreen(graph.crashLog, entry.arguments?.getString("at")?.toLongOrNull() ?: 0L, onBack = back)
        }
        page(SettingsRoutes.LICENSES) { LicensesScreen(onBack = back) }
    }

    // 节点的各个二级页面。窄屏时在主导航里一页一页推进去；宽屏时在节点页右边的那一栏里（[n] 是那一栏自己的导航，
    // 第一页是 NodesPanes 里的节点详情）。
    fun NavGraphBuilder.nodePages(n: NavController, onRemoved: () -> Unit) {
        val back: () -> Unit = { n.popBackStack() }
        val nodeActions = object : NodeActions {
            override fun back() { n.popBackStack() }
            override fun openContainer(projectId: String, name: String) = n.navigate("container/$projectId/$name")
            override fun removed() = onRemoved()
        }
        page("node/{id}") { entry -> NodeScreen(nodes, entry.arguments?.getString("id").orEmpty(), nodeActions) }
        page("nodes/add") { AddNodeScreen(nodes.repo, onBack = back, onDone = { nodes.refresh(); back() }) }
    }
    val nodesState by nodes.list.collectAsStateWithLifecycle()
    val canAddNode = nodes.accessOf(nodesState)?.isAdmin == true

    val panel by session.current.collectAsStateWithLifecycle()
    Row(Modifier.fillMaxSize()) {
        if (wide) {
            // 左栏：上面是自己的头像（点开面板列表，代替顶部栏），左下角是竖着的胶囊，离底部手势条 12（和底部的胶囊一样）。
            // 右边整块都给内容，没有顶部栏。胶囊选中的一项会横着浮出到右边，所以左栏在内容的上面一层。
            val info by settings.info.collectAsStateWithLifecycle()
            Column(
                Modifier.zIndex(1f).width(88.dp).fillMaxHeight().statusBarsPadding().navigationBarsPadding().padding(top = 12.dp, bottom = 12.dp),
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                val me = info?.displayName?.ifEmpty { null } ?: info?.username ?: session.panel.displayName.ifEmpty { session.panel.username }
                PanelBadge(panel.title, me, info?.avatar?.toByteArray()) { sheet = true }
                Spacer(Modifier.weight(1f))
                PillRail(navItems, tab.ordinal, selectTab)
            }
        }
        NavHost(
            nav, startDestination = "tabs",
            modifier = Modifier.weight(1f),
            enterTransition = { NagiTransitions.pushIn(reduced) },
            exitTransition = { NagiTransitions.pushOut(reduced) },
            popEnterTransition = { NagiTransitions.popIn(reduced) },
            popExitTransition = { NagiTransitions.popOut(reduced) },
        ) {
            page("tabs") {
                Box(Modifier.fillMaxSize()) {
                    // 内容铺满整页，从顶部栏下面开始（上内边距是状态栏加顶部栏的高度），往上滚时从顶部栏下面经过；
                    // 顶部栏自己的下边缘渐变成透明（fade），内容滚到那里变淡。窄屏时底部留出胶囊的高度。
                    // 宽屏没有顶部栏，内容从状态栏下面 12 开始。
                    val statusTop = WindowInsets.statusBars.asPaddingValues().calculateTopPadding()
                    val top = statusTop + if (wide) 12.dp else Dimens.topBarHeight
                    val contentPadding = PaddingValues(top = top, bottom = if (wide) 24.dp else Dimens.bottomFade + 24.dp)
                    // 主导航切换：跟导航同方向左右滑入并淡入（选右边的标签，新页面从右边进来，旧页面往左让开），
                    // 和网页「点导航后的内容」一样：进来 32dp、透明度先到（120ms），共 300ms；旧的同方向移开 16dp、100ms 淡出。
                    // 「移除动画」时只淡入淡出。
                    val density = LocalDensity.current
                    AnimatedContent(
                        targetState = tab,
                        modifier = Modifier.fillMaxSize(),
                        transitionSpec = {
                            val inPx = with(density) { 32.dp.roundToPx() }
                            val outPx = with(density) { 16.dp.roundToPx() }
                            NagiTransitions.tab(targetState.ordinal > initialState.ordinal, inPx, outPx, reduced, vertical = wide)
                        },
                        label = "tab",
                    ) { t ->
                        when (t) {
                            Tab.HOME -> HomeContent(home, actions, contentPadding, showMessage, homeList)
                            Tab.PROJECTS -> Placeholder()
                            Tab.NODES -> if (wide) {
                                NodesPanes(contentPadding, nodes, pickedNode, { pickedNode = it }, pages = { n -> nodePages(n) { pickedNode = null; n.popBackStack(NODE_DETAIL, inclusive = false) } }) { selected, open, add ->
                                    NodesContent(nodes, contentPadding, onOpen = open, onAdd = add, showMessage = showMessage, listState = nodesList, selected = selected, header = true)
                                }
                            } else {
                                NodesContent(nodes, contentPadding, onOpen = { nav.navigate("node/$it") }, onAdd = { nav.navigate("nodes/add") }, showMessage = showMessage, listState = nodesList)
                            }
                            Tab.SETTINGS -> if (wide) {
                                SettingsPanes(contentPadding, pages = { n -> settingsPages(n) }) { detail, open ->
                                    SettingsContent(settings, session, graph.prefs, BuildConfig.VERSION_NAME, contentPadding, settingsScroll, selected = detail, open = open)
                                }
                            } else {
                                SettingsContent(settings, session, graph.prefs, BuildConfig.VERSION_NAME, contentPadding, settingsScroll) { nav.navigate(it) }
                            }
                        }
                    }
                    if (wide) {
                        // 状态栏那一条：背景色往下渐变成透明，内容滚到那里变淡。
                        val bg = NagiTheme.colors.background
                        Box(Modifier.fillMaxWidth().height(statusTop + 12.dp).background(Brush.verticalGradient(0f to bg, 0.6f to bg, 1f to bg.copy(alpha = 0f))))
                    } else {
                        NagiTopBar(panel.title, onTitleClick = { sheet = true }, showChevron = true, fade = true) {
                            // 新建的入口是顶部栏的加号（docs/android.md「新建」）：节点页给管理员添加节点。
                            if (tab == Tab.NODES && canAddNode) NagiIconButton(NagiIcons.Plus, stringResource(ink.sipc.nagi.nodes.R.string.add_node), { nav.navigate("nodes/add") })
                        }
                    }
                    Column(Modifier.align(Alignment.BottomCenter)) {
                        SnackbarHost(snackbar) { Snackbar(it, containerColor = NagiTheme.colors.primary, contentColor = NagiTheme.colors.primaryForeground) }
                        if (!wide) PillNav(navItems, selected = tab.ordinal, onSelect = selectTab)
                    }
                }
            }
            settingsPages(nav)
            nodePages(nav) { nav.popBackStack("tabs", inclusive = false) }
            page("container/{project}/{name}") { entry -> ContainerPlaceholder(entry.arguments?.getString("name").orEmpty()) { nav.popBackStack() } }
        }
    }

    if (sheet) {
        ModalBottomSheet(onDismissRequest = { sheet = false }, containerColor = NagiTheme.colors.card) {
            // 点顶部栏的面板名称：当前面板和退出（多个面板以后再做）。
            val info by settings.info.collectAsStateWithLifecycle()
            PanelDetails(session, logout, Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 16.dp), showAddress = info?.role == "admin")
        }
    }
}

/**
 * 宽屏的设置页：左边是设置的菜单（宽 360），右边是选中的一项，有自己的导航（子页里再点进去也在右边）。
 * 默认显示第一项个人资料，和网页一样。左边选中的一项有底色。
 */
@Composable
private fun SettingsPanes(
    contentPadding: PaddingValues,
    pages: NavGraphBuilder.(NavController) -> Unit,
    menu: @Composable (selected: String?, open: (String) -> Unit) -> Unit,
) {
    val detail = rememberNavController()
    val entry by detail.currentBackStackEntryAsState()
    Row(Modifier.fillMaxSize()) {
        Box(Modifier.width(360.dp).fillMaxHeight()) {
            menu(entry?.destination?.route) { route ->
                // 换一项时清掉右边原来的页面栈，不叠在一起。
                detail.navigate(route) {
                    popUpTo(detail.graph.id) { inclusive = true }
                    launchSingleTop = true
                }
            }
        }
        Box(Modifier.width(1.dp).fillMaxHeight().padding(top = contentPadding.calculateTopPadding()).background(NagiTheme.colors.border))
        // 右边一栏在主顶部栏下面，子页自己的顶部栏（返回、标题）不再留状态栏的高度。
        Box(Modifier.weight(1f).fillMaxHeight().padding(top = contentPadding.calculateTopPadding()).consumeWindowInsets(WindowInsets.statusBars)) {
            NavHost(
                detail, startDestination = SettingsRoutes.PROFILE,
                enterTransition = { fadeIn(tween(Motion.FADE_IN)) }, exitTransition = { fadeOut(tween(100)) },
                popEnterTransition = { fadeIn(tween(Motion.FADE_IN)) }, popExitTransition = { fadeOut(tween(100)) },
            ) { pages(detail) }
        }
    }
}

/** 宽屏节点页右边一栏的第一页（选中的节点）。 */
private const val NODE_DETAIL = "detail"

/**
 * 宽屏的节点页：左边是节点列表（宽 360），右边是选中的节点（[picked]，没有选时是列表里的第一个），有自己的导航
 * （添加节点、容器也在右边）。左边选中的一行有底色。
 */
@Composable
private fun NodesPanes(
    contentPadding: PaddingValues,
    vm: NodesViewModel,
    picked: String?,
    pick: (String?) -> Unit,
    pages: NavGraphBuilder.(NavController) -> Unit,
    list: @Composable (selected: String?, open: (String) -> Unit, add: () -> Unit) -> Unit,
) {
    val detail = rememberNavController()
    val state by vm.list.collectAsStateWithLifecycle()
    val ids = (state as? Polled.Ready)?.value?.nodes?.map { it.id }.orEmpty()
    // 选中的节点不在了（被移除）时看第一个。
    val shown = picked?.takeIf { it in ids } ?: ids.firstOrNull()
    Row(Modifier.fillMaxSize()) {
        Box(Modifier.width(360.dp).fillMaxHeight()) {
            list(shown, { id ->
                pick(id)
                detail.popBackStack(NODE_DETAIL, inclusive = false)
            }, { detail.navigate("nodes/add") { launchSingleTop = true } })
        }
        Box(Modifier.width(1.dp).fillMaxHeight().padding(top = contentPadding.calculateTopPadding()).background(NagiTheme.colors.border))
        Box(Modifier.weight(1f).fillMaxHeight().padding(top = contentPadding.calculateTopPadding()).consumeWindowInsets(WindowInsets.statusBars)) {
            NavHost(
                detail, startDestination = NODE_DETAIL,
                enterTransition = { fadeIn(tween(Motion.FADE_IN)) }, exitTransition = { fadeOut(tween(100)) },
                popEnterTransition = { fadeIn(tween(Motion.FADE_IN)) }, popExitTransition = { fadeOut(tween(100)) },
            ) {
                page(NODE_DETAIL) {
                    // 换一个节点时淡入淡出，不从右边滑进来。
                    AnimatedContent(
                        targetState = shown,
                        transitionSpec = { fadeIn(tween(Motion.FADE_IN)) togetherWith fadeOut(tween(100)) },
                        label = "node",
                    ) { id ->
                        when {
                            id != null -> NodeScreen(vm, id, object : NodeActions {
                                override fun back() {}
                                override fun openContainer(projectId: String, name: String) = detail.navigate("container/$projectId/$name")
                                override fun removed() = pick(null)
                            }, showBack = false)
                            state is Polled.Loading -> Box(Modifier.fillMaxSize())
                            else -> EmptyState(stringResource(ink.sipc.nagi.nodes.R.string.nodes_none_selected))
                        }
                    }
                }
                pages(detail)
                page("container/{project}/{name}") { entry -> ContainerPlaceholder(entry.arguments?.getString("name").orEmpty()) { detail.popBackStack() } }
            }
        }
    }
}

/** 容器页还没有做：返回、名称和一行说明。 */
@Composable
private fun ContainerPlaceholder(name: String, onBack: () -> Unit) {
    Column(Modifier.fillMaxSize().navigationBarsPadding()) {
        NagiBackTopBar(name, onBack = onBack)
        Text(
            stringResource(R.string.placeholder_update), style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground,
            modifier = Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 16.dp),
        )
    }
}

/** 宽屏左栏顶上：自己的头像（没有时是首字母加渐变底），点开面板列表。读屏读出当前面板的名称。 */
@Composable
private fun PanelBadge(title: String, name: String, avatar: ByteArray?, onClick: () -> Unit) {
    Box(
        Modifier.size(48.dp).clip(CircleShape)
            .clickable(onClickLabel = title, role = Role.Button, onClick = onClick)
            .semantics { contentDescription = title },
        contentAlignment = Alignment.Center,
    ) {
        Avatar(name, avatar, size = 44.dp)
    }
}

/** 项目这一版是占位页。 */
@Composable
private fun Placeholder() = EmptyState(stringResource(R.string.placeholder))
