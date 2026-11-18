package io.github.sipc.nagi

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
import io.github.sipc.nagi.data.AppGraph
import io.github.sipc.nagi.data.OverviewState
import io.github.sipc.nagi.data.PanelSession
import io.github.sipc.nagi.home.HomeActions
import io.github.sipc.nagi.home.HomeContent
import io.github.sipc.nagi.home.HomeViewModel
import io.github.sipc.nagi.network.PairingUri
import io.github.sipc.nagi.pairing.ConfirmScreen
import io.github.sipc.nagi.pairing.PairingViewModel
import io.github.sipc.nagi.pairing.ScanScreen
import io.github.sipc.nagi.pairing.UnlockScreen
import io.github.sipc.nagi.pairing.WelcomeScreen
import io.github.sipc.nagi.proto.v1.AttentionItem
import io.github.sipc.nagi.ui.BottomFade
import io.github.sipc.nagi.ui.ButtonVariant
import io.github.sipc.nagi.ui.Dimens
import io.github.sipc.nagi.ui.NagiBackTopBar
import io.github.sipc.nagi.ui.NagiButton
import io.github.sipc.nagi.ui.NagiIcons
import io.github.sipc.nagi.ui.NagiTheme
import io.github.sipc.nagi.ui.NagiTopBar
import io.github.sipc.nagi.ui.PillNav
import io.github.sipc.nagi.ui.PillNavItem
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
            s == null -> PairingFlow(graph, pendingPair, removed) {
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
private fun PairingFlow(graph: AppGraph, pendingPair: MutableStateFlow<PairingUri?>, removed: Boolean, onDone: () -> Unit) {
    val nav = rememberNavController()
    val vm: PairingViewModel = viewModel(factory = Factory { PairingViewModel(graph) })
    val pending by pendingPair.collectAsState()
    LaunchedEffect(pending) {
        val uri = pending ?: return@LaunchedEffect
        pendingPair.value = null
        vm.open(uri)
        nav.navigate("confirm") { launchSingleTop = true }
    }
    NavHost(nav, startDestination = "welcome") {
        composable("welcome") {
            WelcomeScreen(onScan = { nav.navigate("scan") }, notice = if (removed) stringResource(R.string.removed) else null)
        }
        composable("scan") {
            ScanScreen(onBack = { nav.popBackStack() }, onScanned = { uri ->
                vm.open(uri)
                nav.navigate("confirm") { popUpTo("welcome") }
            })
        }
        composable("confirm") {
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

    NavHost(nav, startDestination = "tabs") {
        composable("tabs") {
            Box(Modifier.fillMaxSize()) {
                Column(Modifier.fillMaxSize()) {
                    NagiTopBar(session.panel.name, onTitleClick = { sheet = true }, showChevron = true)
                    val bottom = PaddingValues(bottom = Dimens.bottomFade + 24.dp)
                    when (tab) {
                        Tab.HOME -> HomeContent(home, actions, bottom, showMessage)
                        Tab.PROJECTS -> Placeholder()
                        Tab.NODES -> Placeholder()
                        Tab.SETTINGS -> Placeholder()
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
