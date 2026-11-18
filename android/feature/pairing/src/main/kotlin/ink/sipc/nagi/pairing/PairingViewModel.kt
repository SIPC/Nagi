package ink.sipc.nagi.pairing

import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ink.sipc.nagi.data.AppGraph
import ink.sipc.nagi.network.PairingUri
import ink.sipc.nagi.network.UnreachableException
import ink.sipc.nagi.proto.v1.GetPairingResponse
import ink.sipc.nagi.security.AuthCancelledException
import ink.sipc.nagi.security.Biometric
import ink.sipc.nagi.security.KeyGenerationException
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch

/** 确认添加页的状态。 */
data class ConfirmState(
    val uri: PairingUri? = null,
    val info: GetPairingResponse? = null,
    val loading: Boolean = false,
    val submitting: Boolean = false,
    val error: String? = null,
    /** 查询配对码时连不上面板（不是面板返回的错误）。 */
    val unreachable: Boolean = false,
    val done: Boolean = false,
)

class PairingViewModel(private val graph: AppGraph) : ViewModel() {
    private val _state = MutableStateFlow(ConfirmState())
    val state: StateFlow<ConfirmState> = _state

    val deviceLabel: String get() = graph.pairing.deviceLabel()

    /** 扫到（或从链接打开）二维码以后查询配对码。 */
    fun open(uri: PairingUri) {
        if (_state.value.uri == uri && (_state.value.info != null || _state.value.loading)) return
        _state.value = ConfirmState(uri = uri, loading = true)
        lookup()
    }

    fun retry() = lookup()

    private fun lookup() {
        val uri = _state.value.uri ?: return
        _state.value = _state.value.copy(loading = true, error = null)
        viewModelScope.launch {
            try {
                val info = graph.pairing.lookup(uri)
                _state.value = _state.value.copy(info = info, loading = false)
            } catch (e: Exception) {
                _state.value = _state.value.copy(loading = false, error = e.message, unreachable = e is UnreachableException)
            }
        }
    }

    /** 点「添加」：检查锁屏、生成密钥、验证身份、提交。 */
    fun submit(activity: FragmentActivity, texts: PairingTexts) {
        val s = _state.value
        val uri = s.uri ?: return
        val info = s.info ?: return
        if (s.submitting) return
        if (!Biometric.available(activity)) {
            _state.value = s.copy(error = texts.noLock)
            return
        }
        _state.value = s.copy(submitting = true, error = null)
        viewModelScope.launch {
            try {
                val panel = graph.pairing.pair(uri, info) { panelId ->
                    // 用确认密钥签一次：确认是机主在操作，也确认确认密钥能用。
                    val sig = Biometric.authenticate(
                        activity, graph.keys.confirmSignature(panelId), texts.promptTitle(info.panelName), texts.promptSubtitle, texts.cancel,
                    )
                    sig.update(uri.code.toByteArray())
                    sig.sign()
                }
                graph.use(panel)
                _state.value = _state.value.copy(submitting = false, done = true)
            } catch (e: AuthCancelledException) {
                _state.value = _state.value.copy(submitting = false)
            } catch (e: KeyGenerationException) {
                _state.value = _state.value.copy(submitting = false, error = texts.keyFailed(e.message.orEmpty()))
            } catch (e: Exception) {
                _state.value = _state.value.copy(submitting = false, error = e.message)
            }
        }
    }
}

/** 系统验证框和错误里用到的文字（从字符串资源取）。 */
class PairingTexts(
    val promptTitle: (String) -> String,
    val promptSubtitle: String,
    val cancel: String,
    val noLock: String,
    val keyFailed: (String) -> String,
)
