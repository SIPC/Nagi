package ink.sipc.nagi.ui

import android.graphics.BitmapFactory
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import android.util.LruCache
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

// 头像和图标底（见 docs/android-ui.md#头像和图标底）。

/** 头像解码后长边最多这么多像素：显示最大 56dp，按 4 倍密度算是 224。 */
internal const val AVATAR_MAX_PX = 256

/**
 * 解码头像时的缩小倍数（BitmapFactory 的 inSampleSize，2 的幂）：缩到长边不超过 [target]。
 * 头像来自面板，任何成员都能上传，256 KB 的 PNG 能写成 5400×5400，原样解出来上百 MB，绘制时 App 崩溃（v0.35.0#17）。
 * 尺寸读不出来（不是图片）时返回 0。
 */
internal fun avatarSampleSize(width: Int, height: Int, target: Int = AVATAR_MAX_PX): Int {
    if (width <= 0 || height <= 0 || target <= 0) return 0
    val side = maxOf(width, height)
    var sample = 1
    while (side / sample > target && sample < (1 shl 30)) sample *= 2
    return sample
}

/** 先只读尺寸，再按 [avatarSampleSize] 缩小解码。解不开时返回 null，显示默认头像。 */
private fun decodeAvatar(image: ByteArray): ImageBitmap? = runCatching {
    val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
    BitmapFactory.decodeByteArray(image, 0, image.size, bounds)
    val sample = avatarSampleSize(bounds.outWidth, bounds.outHeight)
    if (sample == 0) return@runCatching null
    val opts = BitmapFactory.Options().apply { inSampleSize = sample }
    BitmapFactory.decodeByteArray(image, 0, image.size, opts)?.asImageBitmap()
}.getOrNull()

/** 解过的头像（按内容核对，不只看哈希），列表滚动、刷新时不重新解码，也不闪一下默认头像。 */
private val avatarCache = LruCache<Int, Pair<ByteArray, ImageBitmap>>(32)

private fun cachedAvatar(image: ByteArray): ImageBitmap? =
    avatarCache.get(image.contentHashCode())?.takeIf { it.first.contentEquals(image) }?.second

/**
 * Avatar：成员头像。[image] 是面板给的图片（PNG、JPEG、WebP），在后台线程缩小解码；解不开、没有或还没解完时
 * 显示名称首字母加渐变底，和网页的默认头像一样。
 */
@Composable
fun Avatar(name: String, image: ByteArray?, modifier: Modifier = Modifier, size: Dp = 40.dp) {
    val cached = remember(image) { image?.takeIf { it.isNotEmpty() }?.let(::cachedAvatar) }
    val bitmap by produceState(cached, image) {
        if (image == null || image.isEmpty()) { value = null; return@produceState }
        value = cachedAvatar(image) ?: withContext(Dispatchers.Default) { decodeAvatar(image) }?.also {
            avatarCache.put(image.contentHashCode(), image to it)
        }
    }
    val b = bitmap
    if (b != null) {
        Image(b, contentDescription = null, contentScale = ContentScale.Crop, modifier = modifier.size(size).clip(CircleShape))
    } else {
        Box(modifier.size(size).background(AvatarGradient, CircleShape), contentAlignment = Alignment.Center) {
            Text(name.take(1).uppercase(), style = NagiTheme.type.bodyMedium, color = Color.White)
        }
    }
}

/** IconCircle：圆形 muted 底上的一个图标（解锁、空状态、结果页的头图）。 */
@Composable
fun IconCircle(icon: ImageVector, modifier: Modifier = Modifier, size: Dp = 40.dp, iconSize: Dp = 18.dp) {
    Box(modifier.size(size).background(NagiTheme.colors.muted, CircleShape), contentAlignment = Alignment.Center) {
        Icon(icon, contentDescription = null, tint = NagiTheme.colors.foreground, modifier = Modifier.size(iconSize))
    }
}

private val AvatarGradient = Brush.linearGradient(listOf(Color(0xFF8B5CF6), Color(0xFFEC4899)))
