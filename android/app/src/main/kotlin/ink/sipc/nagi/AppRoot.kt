package ink.sipc.nagi

import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.slideInVertically
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.animation.EnterTransition
import androidx.compose.animation.ExitTransition
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.togetherWith
import ink.sipc.nagi.ui.Motion
import ink.sipc.nagi.ui.isReducedMotion
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
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
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
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
import ink.sipc.nagi.ui.BottomFade
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiButton
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
    Box(Modifier.fillMaxSize().background(NagiTheme.colors.background)) {
        val s = session
        when {
            s == null -> PairingFlow(graph, pendingPair, removed, onPaired = lock::unlocked) {
                removed = false
                lock.unlocked()
            }
            locked != null -> UnlockScreen(s, locked?.idleMinutes, onUnlocked = lock::unlocked, onRemoved = {
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
        enterTransition = { pushIn(reduced) },
        exitTransition = { pushOut(reduced) },
        popEnterTransition = { popIn(reduced) },
        popExitTransition = { popOut(reduced) },
    ) {
        composable("welcome") {
            WelcomeScreen(onScan = { nav.navigate("scan") }, notice = if (removed) stringResource(R.string.removed) else null)
        }
        // 扫码成功时取景框已经展开成整页底色，确认页直接淡入接上，不再从右边滑入；扫码页同时淡出。
        composable(
            "scan",
            // 去确认页时扫码页不淡出：它这时已经铺满页面底色，确认页叠在上面淡入的这段时间（240ms）一直完整地垫在下面，
            // 确认页完全显示后才移除。两页同时半透明会透出窗口的背景。
            exitTransition = { if (targetState.destination.route == "confirm") fadeOut(tween(1, delayMillis = 260)) else pushOut(reduced) },
        ) {
            ScanScreen(
                onBack = { nav.popBackStack() },
                // 锁定二维码时就开始查配对信息，展开动画期间数据就到了。
                onLocked = { uri -> vm.open(uri) },
                onScanned = { nav.navigate("confirm") { popUpTo("welcome") } },
            )
        }
        composable(
            "confirm",
            enterTransition = {
                // 从扫码页来：扫码页已经展开成同样的底色，确认页的内容在上面淡入、轻轻上移 8dp。
                if (initialState.destination.route == "scan") {
                    if (reduced) fadeIn(tween(Motion.FADE_IN))
                    else fadeIn(tween(240, easing = Motion.Enter)) + slideInVertically(tween(320, easing = Motion.ContentIn)) { rise }
                } else {
                    pushIn(reduced)
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
    val snackbar = remember { SnackbarHostState() }
    val scope = rememberCoroutineScope()
    var sheet by remember { mutableStateOf(false) }
    val home: HomeViewModel = viewModel(key = session.panel.id, factory = Factory { HomeViewModel(session) })
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
        enterTransition = { pushIn(reduced) },
        exitTransition = { pushOut(reduced) },
        popEnterTransition = { popIn(reduced) },
        popExitTransition = { popOut(reduced) },
    ) {
        composable("tabs") {
            Box(Modifier.fillMaxSize()) {
                Column(Modifier.fillMaxSize()) {
                    NagiTopBar(session.panel.name, onTitleClick = { sheet = true }, showChevron = true)
                    val bottom = PaddingValues(bottom = Dimens.bottomFade + 24.dp)
                    // 主导航切换：跟导航同方向左右滑入并淡入（选右边的标签，新页面从右边进来，旧页面往左让开），
                    // 和网页「点导航后的内容」一样：进来 32dp、透明度先到（120ms），共 300ms；旧的同方向移开 16dp、100ms 淡出。
                    // 「移除动画」时只淡入淡出。
                    val reducedTabs = isReducedMotion()
                    val density = LocalDensity.current
                    AnimatedContent(
                        targetState = tab,
                        transitionSpec = {
                            val dir = if (targetState.ordinal > initialState.ordinal) 1 else -1
                            val inPx = with(density) { 32.dp.roundToPx() }
                            val outPx = with(density) { 16.dp.roundToPx() }
                            if (reducedTabs) {
                                fadeIn(tween(Motion.FADE_IN)) togetherWith fadeOut(tween(100))
                            } else {
                                (slideInHorizontally(tween(300, easing = Motion.ContentIn)) { dir * inPx } + fadeIn(tween(120, easing = Motion.Enter))) togetherWith
                                    (slideOutHorizontally(tween(100, easing = Motion.Exit)) { -dir * outPx } + fadeOut(tween(100, easing = Motion.Exit)))
                            }
                        },
                        label = "tab",
                    ) { t ->
                        when (t) {
                            Tab.HOME -> HomeContent(home, actions, bottom, showMessage)
                            Tab.PROJECTS -> Placeholder()
                            Tab.NODES -> Placeholder()
                            Tab.SETTINGS -> Placeholder()
                        }
                    }
                }
                BottomFade(Modifier.align(Alignment.BottomCenter))
                Column(Modifier.align(Alignment.BottomCenter)) {
                    SnackbarHost(snackbar) { Snackbar(it, containerColor = NagiTheme.colors.primary, contentColor = NagiTheme.colors.primaryForeground) }
                    PillNav(
                        listOf(
                            PillNavItem(NagiIcons.Home, stringResource(R.string.tab_home), badge = critical),
                            PillNavItem(NagiIcons.Folder, stringResource(R.string.tab_projects)),
                            PillNavItem(NagiIcons.Server, stringResource(R.string.tab_nodes)),
                            PillNavItem(NagiIcons.Settings, stringResource(R.string.tab_settings)),
                        ),
                        selected = tab.ordinal, onSelect = { tab = Tab.entries[it] },
                    )
                }
            }
        }
        composable("container/{project}/{name}") { entry ->
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
            PanelSheet(session, onLogout = {
                scope.launch {
                    runCatching { session.logout() }
                    sheet = false
                    onRemoved()
                }
            })
        }
    }
}

/** 点顶部栏的面板名称：当前面板和退出（多个面板以后再做）。 */
@Composable
private fun PanelSheet(session: PanelSession, onLogout: () -> Unit) {
    var confirming by remember { mutableStateOf(false) }
    Column(Modifier.fillMaxWidth().padding(horizontal = Dimens.pageHorizontal).navigationBarsPadding().padding(bottom = 16.dp)) {
        Text(session.panel.name, style = NagiTheme.type.section, color = NagiTheme.colors.foreground)
        Spacer(Modifier.height(4.dp))
        val member = session.panel.displayName.ifEmpty { session.panel.username }
        Text(stringResource(R.string.panel_member, member), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
        Text(session.panel.address, style = NagiTheme.type.mono, color = NagiTheme.colors.mutedForeground)
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

/** 项目、节点、设置这一版是占位页。 */
@Composable
private fun Placeholder() {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        Text(stringResource(R.string.placeholder), style = NagiTheme.type.body, color = NagiTheme.colors.mutedForeground)
    }
}

// 进下一级：新页面从右边滑入，旧页面往左让一点并淡出；返回反过来（docs/android.md 交互「动画」）。
// 「移除动画」时只淡入淡出。
private fun pushIn(reduced: Boolean): EnterTransition =
    if (reduced) fadeIn(tween(Motion.FADE_IN)) else slideInHorizontally(tween(300, easing = Motion.ContentIn)) { it } + fadeIn(tween(120))

private fun pushOut(reduced: Boolean): ExitTransition =
    if (reduced) fadeOut(tween(100)) else slideOutHorizontally(tween(300, easing = Motion.ContentIn)) { -it / 4 } + fadeOut(tween(300))

private fun popIn(reduced: Boolean): EnterTransition =
    if (reduced) fadeIn(tween(Motion.FADE_IN)) else slideInHorizontally(tween(300, easing = Motion.ContentIn)) { -it / 4 } + fadeIn(tween(300))

private fun popOut(reduced: Boolean): ExitTransition =
    if (reduced) fadeOut(tween(100)) else slideOutHorizontally(tween(300, easing = Motion.ContentIn)) { it } + fadeOut(tween(100, 200))
