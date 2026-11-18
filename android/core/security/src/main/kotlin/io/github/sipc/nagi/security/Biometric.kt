package io.github.sipc.nagi.security

import android.os.Build
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricManager.Authenticators.BIOMETRIC_STRONG
import androidx.biometric.BiometricManager.Authenticators.DEVICE_CREDENTIAL
import androidx.biometric.BiometricPrompt
import androidx.core.content.ContextCompat
import androidx.fragment.app.FragmentActivity
import java.security.Signature
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException
import kotlinx.coroutines.suspendCancellableCoroutine

/** 用户取消了系统验证框。 */
class AuthCancelledException : Exception("cancelled")

/** 系统验证失败（例如次数过多被锁定）。 */
class AuthFailedException(message: String) : Exception(message)

// 系统的验证框（BiometricPrompt）：用确认密钥签名前验证指纹、面容或锁屏密码。
object Biometric {
    /** 这台手机能不能用确认密钥：Android 11 起指纹、面容或锁屏密码都行，9、10 只能用指纹或面容。 */
    fun available(activity: FragmentActivity): Boolean =
        BiometricManager.from(activity).canAuthenticate(authenticators()) == BiometricManager.BIOMETRIC_SUCCESS

    private fun authenticators(): Int =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) BIOMETRIC_STRONG or DEVICE_CREDENTIAL else BIOMETRIC_STRONG

    /**
     * 弹出验证框，通过后返回可以签名的 Signature。
     * [cancelText] 是 Android 9、10 上的取消按钮（11 起有锁屏密码选项，没有取消按钮）。
     */
    suspend fun authenticate(activity: FragmentActivity, signature: Signature, title: String, subtitle: String?, cancelText: String): Signature =
        suspendCancellableCoroutine { cont ->
            val prompt = BiometricPrompt(activity, ContextCompat.getMainExecutor(activity), object : BiometricPrompt.AuthenticationCallback() {
                override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
                    val s = result.cryptoObject?.signature
                    if (s == null) cont.resumeWithException(AuthFailedException("no signature")) else cont.resume(s)
                }

                override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
                    val cancelled = errorCode == BiometricPrompt.ERROR_USER_CANCELED || errorCode == BiometricPrompt.ERROR_NEGATIVE_BUTTON ||
                        errorCode == BiometricPrompt.ERROR_CANCELED
                    cont.resumeWithException(if (cancelled) AuthCancelledException() else AuthFailedException(errString.toString()))
                }
            })
            val info = BiometricPrompt.PromptInfo.Builder()
                .setTitle(title)
                .apply { if (subtitle != null) setSubtitle(subtitle) }
                .setAllowedAuthenticators(authenticators())
                .apply { if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) setNegativeButtonText(cancelText) }
                .build()
            prompt.authenticate(info, BiometricPrompt.CryptoObject(signature))
            cont.invokeOnCancellation { prompt.cancelAuthentication() }
        }
}
