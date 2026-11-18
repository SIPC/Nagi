package ink.sipc.nagi

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
import ink.sipc.nagi.settings.SoonScreen
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
import ink.sipc.nagi.proto.v1.AttentionItem
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
            else -> MainTabs(graph, s, onLocked = lock::lockNow, onRemoved = {
                graph.forget()
                removed = true
            })
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
        vm.open(uri)
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
    val critical = (overview as? OverviewState.Ready)?.overview?.attention?.itemsList?.any { it.severity == "critical" } == true
    val showMessage: (String) -> Unit = { msg -> scope.launch { snackbar.showSnackbar(msg) } }
    val actions = object : HomeActions {
        override fun openAttention(item: AttentionItem) {
            when {
                item.container.isNotEmpty() -> nav.navigate("container/${item.projectId}/${item.container}")
                item.nodeId.isNotEmpty() -> tab = Tab.NODES
            }
        }

        override fun openNodes(offlineOnly: Boolean) {
            tab = Tab.NODES
        }

        override fun openProjects(notRunningOnly: Boolean) {
            tab = Tab.PROJECTS
        }

        override fun update(item: AttentionItem) {
            nav.navigate("container/${item.projectId}/${item.container}")
        }
    }

    val reduced = isReducedMotion()
    NavHost(
        nav, startDestination = "tabs",
        enterTransition = { NagiTransitions.pushIn(reduced) },
        exitTransition = { NagiTransitions.pushOut(reduced) },
        popEnterTransition = { NagiTransitions.popIn(reduced) },
        popExitTransition = { NagiTransitions.popOut(reduced) },
    ) {
        page("tabs") {
            Box(Modifier.fillMaxSize()) {
                    // 内容铺满整页，从顶部栏下面开始（上内边距是状态栏加顶部栏的高度），往上滚时从顶部栏下面经过；
                    // 顶部栏自己的下边缘渐变成透明（fade），内容滚到那里变淡。底部留出胶囊的高度。
                    val top = WindowInsets.statusBars.asPaddingValues().calculateTopPadding() + Dimens.topBarHeight
                    val contentPadding = PaddingValues(top = top, bottom = Dimens.bottomFade + 24.dp)
                    // 主导航切换：跟导航同方向左右滑入并淡入（选右边的标签，新页面从右边进来，旧页面往左让开），
                    // 和网页「点导航后的内容」一样：进来 32dp、透明度先到（120ms），共 300ms；旧的同方向移开 16dp、100ms 淡出。
                    // 「移除动画」时只淡入淡出。
                    val reducedTabs = isReducedMotion()
                    val density = LocalDensity.current
                    AnimatedContent(
                        targetState = tab,
                        modifier = Modifier.fillMaxSize(),
                        transitionSpec = {
                            val inPx = with(density) { 32.dp.roundToPx() }
                            val outPx = with(density) { 16.dp.roundToPx() }
                            NagiTransitions.tab(targetState.ordinal > initialState.ordinal, inPx, outPx, reducedTabs)
                        },
                        label = "tab",
                    ) { t ->
                        when (t) {
                            Tab.HOME -> HomeContent(home, actions, contentPadding, showMessage, homeList)
                            Tab.PROJECTS -> Placeholder()
                            Tab.NODES -> Placeholder()
                            Tab.SETTINGS -> SettingsContent(settings, session, graph.prefs, BuildConfig.VERSION_NAME, contentPadding, settingsScroll) { nav.navigate(it) }
                        }
                    }
                val panel by session.current.collectAsStateWithLifecycle()
                NagiTopBar(panel.title, onTitleClick = { sheet = true }, showChevron = true, fade = true)
                Column(Modifier.align(Alignment.BottomCenter)) {
                    SnackbarHost(snackbar) { Snackbar(it, containerColor = NagiTheme.colors.primary, contentColor = NagiTheme.colors.primaryForeground) }
                    PillNav(
                        listOf(
                            PillNavItem(NagiIcons.Home, stringResource(R.string.tab_home), badge = critical),
                            PillNavItem(NagiIcons.Folder, stringResource(R.string.tab_projects)),
                            PillNavItem(NagiIcons.Server, stringResource(R.string.tab_nodes)),
                            PillNavItem(NagiIcons.Settings, stringResource(R.string.tab_settings)),
                        ),
                        selected = tab.ordinal,
                        onSelect = { i ->
                            // 再点一次当前的标签：回到顶部。
                            if (i == tab.ordinal) {
                                scope.launch {
                                    when (tab) {
                                        Tab.HOME -> if (reduced) homeList.scrollToItem(0) else homeList.animateScrollToItem(0)
                                        Tab.SETTINGS -> if (reduced) settingsScroll.scrollTo(0) else settingsScroll.animateScrollTo(0)
                                        else -> {}
                                    }
                                }
                            } else {
                                tab = Tab.entries[i]
                            }
                        },
                    )
                }
            }
        }
        page(SettingsRoutes.PANEL) {
            val info by settings.info.collectAsStateWithLifecycle()
            PanelScreen(session, info?.role == "admin", onBack = { nav.popBackStack() }, onLogout = logout) { nav.navigate(it) }
        }
        page(SettingsRoutes.PANEL_ALIAS) { PanelAliasScreen(session, onBack = { nav.popBackStack() }) }
        page(SettingsRoutes.PANEL_NAME) {
            val defaultName by settings.defaultName.collectAsStateWithLifecycle()
            PanelNameScreen(session, defaultName, onBack = { nav.popBackStack() })
        }
        page(SettingsRoutes.APPEARANCE) { AppearanceScreen(graph.prefs, onBack = { nav.popBackStack() }) }
        page(SettingsRoutes.LANGUAGE) { LanguageScreen(graph.prefs, onBack = { nav.popBackStack() }) }
        page(SettingsRoutes.ABOUT) {
            val info by settings.info.collectAsStateWithLifecycle()
            AboutScreen(BuildConfig.VERSION_NAME, info?.version, onBack = { nav.popBackStack() }) { nav.navigate(it) }
        }
        page(SettingsRoutes.CRASHES) { CrashLogsScreen(graph.crashLog, onBack = { nav.popBackStack() }) { nav.navigate(it) } }
        page(SettingsRoutes.CRASH) { entry ->
            CrashLogScreen(graph.crashLog, entry.arguments?.getString("at")?.toLongOrNull() ?: 0L, onBack = { nav.popBackStack() })
        }
        page(SettingsRoutes.LICENSES) { LicensesScreen(onBack = { nav.popBackStack() }) }
        page(SettingsRoutes.SOON) { entry -> SoonScreen(entry.arguments?.getString("key").orEmpty(), onBack = { nav.popBackStack() }) }
        page("container/{project}/{name}") { entry ->
            val name = entry.arguments?.getString("name").orEmpty()
            Column(Modifier.fillMaxSize().navigationBarsPadding()) {
                NagiBackTopBar(name, onBack = { nav.popBackStack() })
                Text(
                    stringResource(R.string.placeholder_update), style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground,
                    modifier = Modifier.padding(horizontal = Dimens.pageHorizontal, vertical = 16.dp),
                )
            }
        }
    }

    if (sheet) {
        ModalBottomSheet(onDismissRequest = { sheet = false }, containerColor = NagiTheme.colors.card) {
            // 点顶部栏的面板名称：当前面板和退出（多个面板以后再做）。
            PanelDetails(session, logout, Modifier.padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 16.dp))
        }
    }
}

/** 项目、节点这一版是占位页。 */
@Composable
private fun Placeholder() = EmptyState(stringResource(R.string.placeholder))
