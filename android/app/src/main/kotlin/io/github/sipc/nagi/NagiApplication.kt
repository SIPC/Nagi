package io.github.sipc.nagi

import android.app.Application
import android.os.SystemClock
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import io.github.sipc.nagi.data.AppGraph
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

class NagiApplication : Application() {
    lateinit var graph: AppGraph
        private set
    lateinit var lock: AppLock
        private set

    override fun onCreate() {
        super.onCreate()
        graph = AppGraph(this, BuildConfig.VERSION_NAME)
        lock = AppLock(graph)
        ProcessLifecycleOwner.get().lifecycle.addObserver(lock)
    }
}

/**
 * 解锁 App（见 docs/android.md#解锁-app）：打开 App 时、离开前台超过解锁时长后回来时要验证。
 * 面板那边按上次解锁的时间强制，这里只决定什么时候显示解锁页。
 */
class AppLock(private val graph: AppGraph) : DefaultLifecycleObserver {

    /** 锁定的原因：idleMinutes 为空时是刚打开 App。 */
    data class Locked(val idleMinutes: Int?)

    private val _state = MutableStateFlow<Locked?>(Locked(null))
    val locked: StateFlow<Locked?> = _state
    private var stoppedAt = 0L

    override fun onStop(owner: LifecycleOwner) {
        stoppedAt = SystemClock.elapsedRealtime()
    }

    override fun onStart(owner: LifecycleOwner) {
        val session = graph.session.value ?: return
        if (_state.value != null || stoppedAt == 0L) return
        val idle = SystemClock.elapsedRealtime() - stoppedAt
        val limit = session.tokens.unlockMinutes * 60_000L
        if (idle >= limit) {
            session.tokens.invalidate()
            _state.value = Locked((idle / 60_000).toInt())
        }
    }

    fun unlocked() {
        _state.value = null
    }

    /** 面板说要先解锁（例如签名错了几次）。 */
    fun lockNow() {
        graph.session.value?.tokens?.invalidate()
        _state.value = Locked(null)
    }
}
