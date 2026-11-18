package ink.sipc.nagi.security

import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.security.keystore.StrongBoxUnavailableException
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec

// 三把硬件密钥（见 docs/android.md#密钥）：设备密钥换取令牌，确认密钥解锁 App，推送密钥打开告警推送。
// 都是 ECDSA P-256，在 Keystore 里生成、不能导出，有 StrongBox 时优先放在 StrongBox。

/** 密钥的种类。 */
enum class KeyKind(val id: String) {
    DEVICE("device"),
    CONFIRM("confirm"),
    PUSH("push"),
}

/** 一把新生成的密钥：公钥（SPKI）和证明证书链（叶子在前）。 */
class GeneratedKey(val publicKey: ByteArray, val attestation: List<ByteArray>, val strongBox: Boolean)

/** 生成三把密钥时出错（例如手机没有设置锁屏密码）。 */
class KeyGenerationException(message: String, cause: Throwable?) : Exception(message, cause)

class AppKeys {
    private val keyStore: KeyStore = KeyStore.getInstance(PROVIDER).apply { load(null) }

    /** 密钥别名里带面板 ID：每个面板有自己的三把密钥。 */
    fun alias(panelId: String, kind: KeyKind) = "nagi.$panelId.${kind.id}"

    /** 生成三把密钥，证明的挑战是配对码的 SHA-256。已有的同名密钥先删掉。 */
    fun generate(panelId: String, challenge: ByteArray): Map<KeyKind, GeneratedKey> {
        delete(panelId)
        return KeyKind.entries.associateWith { generate(panelId, it, challenge) }
    }

    private fun generate(panelId: String, kind: KeyKind, challenge: ByteArray): GeneratedKey {
        val alias = alias(panelId, kind)
        // 有 StrongBox 时优先，没有（或这种参数不支持）时退回 TEE。
        for (strongBox in listOf(true, false)) {
            try {
                val spec = spec(alias, kind, challenge, strongBox)
                KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, PROVIDER).apply { initialize(spec) }.generateKeyPair()
                val chain = keyStore.getCertificateChain(alias)
                    ?: throw KeyGenerationException("Keystore 没有返回证明证书", null)
                return GeneratedKey(chain[0].publicKey.encoded, chain.map { it.encoded }, strongBox)
            } catch (e: StrongBoxUnavailableException) {
                continue
            } catch (e: KeyGenerationException) {
                throw e
            } catch (e: Exception) {
                if (strongBox) continue
                throw KeyGenerationException(e.message ?: e.javaClass.simpleName, e)
            }
        }
        throw KeyGenerationException("无法生成密钥", null)
    }

    private fun spec(alias: String, kind: KeyKind, challenge: ByteArray, strongBox: Boolean): KeyGenParameterSpec {
        val b = KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .setAttestationChallenge(challenge)
            .setIsStrongBoxBacked(strongBox)
        when (kind) {
            // 手机解锁时才能用。
            KeyKind.DEVICE -> b.setUnlockedDeviceRequired(true)
            // 每次使用都要验证指纹、面容或锁屏密码。Android 11 起可以用锁屏密码。
            KeyKind.CONFIRM -> {
                b.setUserAuthenticationRequired(true)
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                    b.setUserAuthenticationParameters(0, KeyProperties.AUTH_BIOMETRIC_STRONG or KeyProperties.AUTH_DEVICE_CREDENTIAL)
                } else {
                    @Suppress("DEPRECATION")
                    b.setUserAuthenticationValidityDurationSeconds(-1)
                }
            }
            // 锁屏时也能用：只能打开推送连接。
            KeyKind.PUSH -> {}
        }
        return b.build()
    }

    fun has(panelId: String): Boolean = KeyKind.entries.all { keyStore.containsAlias(alias(panelId, it)) }

    fun delete(panelId: String) {
        KeyKind.entries.forEach { runCatching { keyStore.deleteEntry(alias(panelId, it)) } }
    }

    private fun privateKey(panelId: String, kind: KeyKind): PrivateKey =
        keyStore.getKey(alias(panelId, kind), null) as? PrivateKey ?: throw IllegalStateException("没有密钥 ${kind.id}")

    /** 用不需要验证身份的密钥签名（设备密钥、推送密钥）。签名是 DER 格式。 */
    fun sign(panelId: String, kind: KeyKind, data: ByteArray): ByteArray {
        require(kind != KeyKind.CONFIRM) { "确认密钥要经过 BiometricPrompt" }
        return Signature.getInstance(SIGNATURE).run {
            initSign(privateKey(panelId, kind))
            update(data)
            sign()
        }
    }

    /** 确认密钥的 Signature，交给 BiometricPrompt 的 CryptoObject，验证通过后再 update、sign。 */
    fun confirmSignature(panelId: String): Signature =
        Signature.getInstance(SIGNATURE).apply { initSign(privateKey(panelId, KeyKind.CONFIRM)) }

    private companion object {
        const val PROVIDER = "AndroidKeyStore"
        const val SIGNATURE = "SHA256withECDSA"
    }
}
