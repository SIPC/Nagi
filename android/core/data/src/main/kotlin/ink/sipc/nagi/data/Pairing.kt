package ink.sipc.nagi.data

import android.os.Build
import com.google.protobuf.ByteString
import ink.sipc.nagi.network.PairingUri
import ink.sipc.nagi.network.PanelClient
import ink.sipc.nagi.network.orThrow
import ink.sipc.nagi.network.toPanelException
import ink.sipc.nagi.proto.v1.AppDeviceInfo
import ink.sipc.nagi.proto.v1.AppKey
import ink.sipc.nagi.proto.v1.GetPairingRequest
import ink.sipc.nagi.proto.v1.GetPairingResponse
import ink.sipc.nagi.proto.v1.PairRequest
import ink.sipc.nagi.security.AppKeys
import ink.sipc.nagi.security.GeneratedKey
import ink.sipc.nagi.security.KeyKind
import java.security.MessageDigest
import java.util.UUID

// 扫码添加（见 docs/android.md#扫码）：用二维码里的公钥指纹连面板，查询配对码，生成三把密钥并提交证明。

class Pairing(private val keys: AppKeys, private val store: PanelStore, private val appVersion: String) {
    /** 查询配对码：面板名称和成员。 */
    suspend fun lookup(uri: PairingUri): GetPairingResponse = try {
        PanelClient(uri.address, uri.fingerprint).app.getPairing(GetPairingRequest.newBuilder().setCode(uri.code).build()).orThrow()
    } catch (e: Throwable) {
        throw e.toPanelException()
    }

    /** 手机名称和系统版本，显示在确认页和面板的设备列表里。 */
    fun deviceLabel(): String = "${Build.MODEL} · Android ${Build.VERSION.RELEASE}"

    /**
     * 生成三把密钥，提交公钥和证明，保存面板。[beforeSubmit] 在生成密钥后、提交前调用（弹出系统验证框，确认是机主在操作）。
     */
    suspend fun pair(uri: PairingUri, info: GetPairingResponse, beforeSubmit: suspend (panelId: String) -> Unit): Panel {
        val panelId = UUID.randomUUID().toString().replace("-", "").take(12)
        val challenge = MessageDigest.getInstance("SHA-256").digest(uri.code.toByteArray())
        val generated = keys.generate(panelId, challenge)
        try {
            beforeSubmit(panelId)
            fun key(k: GeneratedKey) = AppKey.newBuilder().setPublicKey(ByteString.copyFrom(k.publicKey))
                .addAllAttestation(k.attestation.map { ByteString.copyFrom(it) }).build()
            val req = PairRequest.newBuilder()
                .setCode(uri.code)
                .setDeviceKey(key(generated.getValue(KeyKind.DEVICE)))
                .setConfirmKey(key(generated.getValue(KeyKind.CONFIRM)))
                .setPushKey(key(generated.getValue(KeyKind.PUSH)))
                .setDevice(
                    AppDeviceInfo.newBuilder().setModel(Build.MODEL).setAndroidVersion(Build.VERSION.RELEASE).setAppVersion(appVersion)
                        .setSdk(Build.VERSION.SDK_INT),
                )
                .build()
            val res = try {
                PanelClient(uri.address, uri.fingerprint).app.pair(req).orThrow()
            } catch (e: Throwable) {
                throw e.toPanelException()
            }
            val panel = Panel(
                id = panelId, name = res.panelName.ifEmpty { info.panelName }, address = uri.address, domain = uri.domain,
                fingerprint = uri.fingerprint, deviceId = res.deviceId, username = info.username, displayName = info.displayName,
            )
            store.save(panel)
            return panel
        } catch (e: Throwable) {
            keys.delete(panelId)
            throw e
        }
    }
}
