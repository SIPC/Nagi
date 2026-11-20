package ink.sipc.nagi.network

import com.connectrpc.ResponseMessage
import com.google.protobuf.ByteString
import com.google.protobuf.Timestamp
import ink.sipc.nagi.proto.v1.AppChallengeRequest
import ink.sipc.nagi.proto.v1.AppChallengeResponse
import ink.sipc.nagi.proto.v1.AppServiceClient
import ink.sipc.nagi.proto.v1.AppTokenRequest
import ink.sipc.nagi.proto.v1.AppTokenResponse
import ink.sipc.nagi.security.AppKeys
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Test
import org.mockito.Mockito.mock
import org.mockito.Mockito.`when`

class TokenManagerTest {
    @Test
    fun renewalIntervalIgnoresWallClockChanges() = runBlocking {
        var wallTime = System.currentTimeMillis()
        var elapsedTime = 0L
        val client = mock(PanelClient::class.java)
        val app = mock(AppServiceClient::class.java)
        `when`(client.app).thenReturn(app)
        val challenge = AppChallengeResponse.newBuilder().setChallenge("challenge").build()
        `when`(app.challenge(AppChallengeRequest.newBuilder().setDeviceId("device").build()))
            .thenReturn(ResponseMessage.Success(challenge, emptyMap(), emptyMap()))
        val request = AppTokenRequest.newBuilder()
            .setDeviceId("device").setChallenge("challenge").setKey("confirm").setHost("panel.test")
            .setSignature(ByteString.copyFromUtf8("signature")).build()
        val response = AppTokenResponse.newBuilder()
            .setToken("first-token").setNextChallenge("challenge")
            .setExpiresAt(Timestamp.newBuilder().setSeconds(wallTime / 1000 + 600)).build()
        `when`(app.token(request)).thenReturn(
            ResponseMessage.Success(response, emptyMap(), emptyMap()),
            ResponseMessage.Success(response.toBuilder().setToken("second-token").build(), emptyMap(), emptyMap()),
        )
        val tokens = TokenManager(
            client, mock(AppKeys::class.java), "panel", "device", "panel.test",
            now = { wallTime }, elapsedRealtime = { elapsedTime },
        )

        assertEquals(Long.MAX_VALUE, tokens.sinceExchangeMs())
        assertEquals("first-token", tokens.unlock { "signature".toByteArray() })
        assertEquals(0L, tokens.sinceExchangeMs())

        // 手机时间回拨 30 分钟后，前台仍按实际过去的时间续期。
        elapsedTime += 90_000
        wallTime -= 30 * 60_000
        assertEquals(90_000L, tokens.sinceExchangeMs())
        elapsedTime += 150_000
        wallTime += 150_000
        assertEquals(4 * 60_000L, tokens.sinceExchangeMs())

        // 其他请求成功换取令牌后重新计时，墙钟向前跳也不应提前续期。
        assertEquals("second-token", tokens.unlock { "signature".toByteArray() })
        assertEquals(0L, tokens.sinceExchangeMs())
        elapsedTime += 60_000
        wallTime += 60 * 60_000
        assertEquals(60_000L, tokens.sinceExchangeMs())
    }
}
