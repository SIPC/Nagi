package ink.sipc.nagi.home

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ink.sipc.nagi.data.OverviewState
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.data.TrafficRange
import ink.sipc.nagi.data.TrafficSeries
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch

/** 流量趋势的状态。 */
sealed interface TrafficState {
    data object Loading : TrafficState
    data class Ready(val history: TrafficSeries) : TrafficState
    data class Failed(val message: String) : TrafficState
}

class HomeViewModel(private val session: PanelSession) : ViewModel() {
    /** 首页的数据：页面在前台时每 10 秒查一次（WhileSubscribed），离开 5 秒后停。 */
    val overview: StateFlow<OverviewState> = session.overview()
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), OverviewState.Loading)

    private val _range = MutableStateFlow(TrafficRange.DAY)
    val range: StateFlow<TrafficRange> = _range

    private val _traffic = MutableStateFlow<TrafficState>(TrafficState.Loading)
    val traffic: StateFlow<TrafficState> = _traffic

    /** 操作结果，显示成 Snackbar。 */
    private val _messages = MutableSharedFlow<String>(extraBufferCapacity = 4)
    val messages: SharedFlow<String> = _messages

    private val _approving = MutableStateFlow<Set<String>>(emptySet())
    val approving: StateFlow<Set<String>> = _approving

    private var trafficJob: Job? = null

    init {
        loadTraffic()
    }

    fun setRange(r: TrafficRange) {
        if (_range.value == r) return
        _range.value = r
        loadTraffic()
    }

    /** 历史曲线打开页面和切换时间范围时查一次。 */
    fun loadTraffic() {
        trafficJob?.cancel()
        val r = _range.value
        if (_traffic.value !is TrafficState.Ready) _traffic.value = TrafficState.Loading
        trafficJob = viewModelScope.launch {
            _traffic.value = try {
                TrafficState.Ready(session.traffic(r))
            } catch (e: Exception) {
                TrafficState.Failed(e.message.orEmpty())
            }
        }
    }

    fun refresh() {
        session.refresh()
        loadTraffic()
    }

    fun approve(nodeId: String, name: String, done: (String) -> String) {
        if (nodeId in _approving.value) return
        _approving.value += nodeId
        viewModelScope.launch {
            try {
                session.approveNode(nodeId)
                _messages.emit(done(name))
            } catch (e: Exception) {
                _messages.emit(e.message.orEmpty())
            } finally {
                _approving.value -= nodeId
            }
        }
    }
}
