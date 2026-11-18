package ink.sipc.nagi.ui

import android.graphics.BitmapFactory
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

// 头像和图标底（见 docs/android-ui.md#头像和图标底）。

/**
 * Avatar：成员头像。[image] 是面板给的图片（PNG、JPEG、WebP），解不开或没有时显示名称首字母加渐变底，
 * 和网页的默认头像一样。
 */
@Composable
fun Avatar(name: String, image: ByteArray?, modifier: Modifier = Modifier, size: Dp = 40.dp) {
    val bitmap = remember(image) {
        if (image == null || image.isEmpty()) null
        else runCatching { BitmapFactory.decodeByteArray(image, 0, image.size)?.asImageBitmap() }.getOrNull()
    }
    if (bitmap != null) {
        Image(bitmap, contentDescription = null, contentScale = ContentScale.Crop, modifier = modifier.size(size).clip(CircleShape))
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
