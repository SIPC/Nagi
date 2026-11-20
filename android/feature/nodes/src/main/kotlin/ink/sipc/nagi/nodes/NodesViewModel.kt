package ink.sipc.nagi.nodes

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ink.sipc.nagi.data.NodeList
import ink.sipc.nagi.data.NodeMetrics
import ink.sipc.nagi.data.NodesRepo
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.data.Polled
import ink.sipc.nagi.data.TrafficRange
import ink.sipc.nagi.data.poll
import ink.sipc.nagi.network.UnreachableException
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.proto.v1.ProjectSummary
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch

/**
 * 节点列表（节点页的一级页面和宽屏左边一栏）：和网页一样每 5 秒查一次，页面离开 5 秒后停。
 * 节点详情也从这里取节点（网页的节点页同样用节点列表）。
 */
class NodesViewModel(session: PanelSession) : ViewModel() {
    val repo = NodesRepo(session)
    private val refreshes = Channel<Unit>(Channel.CONFLATED)

    val list: StateFlow<Polled<NodeList>> = poll(5_000, refreshes) { repo.list() }
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), Polled.Loading)

    /** 操作结果，显示成 Snackbar。 */
    private val _messages = MutableSharedFlow<String>(extraBufferCapacity = 4)
    val messages: SharedFlow<String> = _messages

    private val _approving = MutableStateFlow<Set<String>>(emptySet())
    val approving: StateFlow<Set<String>> = _approving

    /** 马上再查一次（重试、做完操作后）。 */
    fun refresh() {
        refreshes.trySend(Unit)
    }

    fun message(text: String) {
        _messages.tryEmit(text)
    }

    /** 批准节点。[done] 是成功时的提示，[unreachable] 是连不上面板时的提示。 */
    fun approve(n: Node, done: String, unreachable: String) {
        if (n.id in _approving.value) return
        _approving.value += n.id
        viewModelScope.launch {
            try {
                repo.approve(n.id)
                _messages.emit(done)
                refresh()
            } catch (e: UnreachableException) {
                _messages.emit(unreachable)
            } catch (e: Exception) {
                _messages.emit(e.message.orEmpty())
            } finally {
                _approving.value -= n.id
            }
        }
    }
}

/** 一个节点的监控和它上面的容器：监控 10 秒、项目列表 5 秒查一次，和网页的节点页一样。 */
@OptIn(ExperimentalCoroutinesApi::class)
class NodeViewModel(private val repo: NodesRepo, private val nodeId: String) : ViewModel() {
    private val _range = MutableStateFlow(TrafficRange.HOUR)
    val range: StateFlow<TrafficRange> = _range
    private val metricsRefresh = Channel<Unit>(Channel.CONFLATED)
    private val projectsRefresh = Channel<Unit>(Channel.CONFLATED)

    val metrics: StateFlow<Polled<NodeMetrics>> = _range
        .flatMapLatest { r -> poll(10_000, metricsRefresh) { repo.metrics(nodeId, r) } }
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), Polled.Loading)

    val projects: StateFlow<Polled<List<ProjectSummary>>> = poll(5_000, projectsRefresh) { repo.projects() }
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), Polled.Loading)

    fun setRange(r: TrafficRange) {
        _range.value = r
    }

    fun retryMetrics() {
        metricsRefresh.trySend(Unit)
    }

    fun retryProjects() {
        projectsRefresh.trySend(Unit)
    }
}
