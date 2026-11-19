package ink.sipc.nagi.ui

import androidx.compose.runtime.staticCompositionLocalOf

/**
 * 高权限确认（见 docs/android.md#高权限确认和重新验证）：面板说这个操作要先验证身份时调用，弹系统验证框、用确认密钥签名。
 * 返回 true 表示确认过了，可以重试刚才的操作；取消时返回 false。主界面提供（AppRoot），没有提供时为 null。
 */
val LocalElevate = staticCompositionLocalOf<(suspend () -> Boolean)?> { null }
