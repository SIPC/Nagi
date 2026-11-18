package ink.sipc.nagi.security

import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

// 缓存加密（见 docs/android.md#缓存）：每个面板一把 AES-256-GCM 密钥，在 Keystore 里生成、不能导出，
// 手机解锁时才能用。放在 TEE（不用 StrongBox）：缓存每次打开都要解密，要快；它保护的只是状态，不是凭据。
// 退出面板时和三把签名密钥一起删除，缓存随之无法解密。

class CacheCipher {
    private val keyStore = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }

    private fun alias(panelId: String) = "nagi.$panelId.cache"

    private fun key(panelId: String): SecretKey {
        (keyStore.getKey(alias(panelId), null) as? SecretKey)?.let { return it }
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
        gen.init(
            KeyGenParameterSpec.Builder(alias(panelId), KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setKeySize(256)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setUnlockedDeviceRequired(true)
                .build(),
        )
        return gen.generateKey()
    }

    /** 加密，返回 IV（12 字节）加密文。 */
    fun encrypt(panelId: String, plain: ByteArray): ByteArray {
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.ENCRYPT_MODE, key(panelId))
        return c.iv + c.doFinal(plain)
    }

    /** 解密；密钥没了、内容被改过时返回 null。 */
    fun decrypt(panelId: String, data: ByteArray): ByteArray? = runCatching {
        if (data.size < 13) return null
        val k = keyStore.getKey(alias(panelId), null) as? SecretKey ?: return null
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.DECRYPT_MODE, k, GCMParameterSpec(128, data, 0, 12))
        c.doFinal(data, 12, data.size - 12)
    }.getOrNull()

    fun delete(panelId: String) {
        runCatching { keyStore.deleteEntry(alias(panelId)) }
    }
}
