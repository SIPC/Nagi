package ink.sipc.nagi.pairing

import androidx.camera.core.FocusMeteringAction
import androidx.camera.core.UseCaseGroup
import androidx.camera.core.resolutionselector.AspectRatioStrategy
import androidx.camera.core.resolutionselector.ResolutionSelector
import androidx.camera.core.resolutionselector.ResolutionStrategy
import java.util.concurrent.TimeUnit
import android.Manifest
import android.util.Range
import android.util.Log
import android.os.Build
import android.view.HapticFeedbackConstants
import androidx.compose.ui.platform.LocalView
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.core.Camera
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.ImageProxy
import androidx.camera.core.Preview
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.view.PreviewView
import androidx.compose.animation.core.Animatable
import androidx.compose.animation.fadeOut
import androidx.compose.animation.fadeIn
import androidx.compose.animation.core.spring
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Rect
import androidx.compose.ui.geometry.RoundRect
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.ClipOp
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.clipPath
import androidx.compose.ui.graphics.lerp
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import com.google.zxing.BarcodeFormat
import com.google.zxing.BinaryBitmap
import com.google.zxing.DecodeHintType
import com.google.zxing.PlanarYUVLuminanceSource
import com.google.zxing.ResultPoint
import com.google.zxing.common.GlobalHistogramBinarizer
import com.google.zxing.common.HybridBinarizer
import com.google.zxing.qrcode.decoder.Decoder
import com.google.zxing.qrcode.detector.Detector
import ink.sipc.nagi.network.PairingUri
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.Motion
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.isReducedMotion
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.util.concurrent.Executors
import kotlin.math.max

/** 扫到的一个二维码：内容，和它在预览画面上的位置（像素）。 */
private class Seen(val text: String, val rect: Rect, val at: Long)

/** 取景框没对准二维码时的边长。 */
private val IdleFrame = 240.dp

/** 二维码离开画面多久，取景框回到中间。 */
private const val LOST_MS = 600L

/** 解出 Nagi 的二维码以后，要已经连续跟踪了这么久才算成功。 */
private const val TRACK_MIN_MS = 150L

/** 对准二维码对焦的最短间隔。 */
private const val FOCUS_EVERY_MS = 1500L

/** 没找到二维码时，多久对画面中心对焦一次。 */
private const val CENTER_FOCUS_MS = 2000L

/** 扫码成功后在二维码上停多久，再展开。 */
private const val LOCK_HOLD_MS = 80L

/** 从二维码展开到全屏的时长。展开结束时框里已经全是页面底色，这时再进确认页。 */
private const val EXPAND_MS = 380

/** 展开到这个比例时开始铺页面底色，展开结束时铺满。 */
private const val FILL_FROM = 0.35f

/** 跟踪时新位置的权重：每帧的定位有几像素的抖动，和上一次的位置做加权平均，框不发抖。 */
private const val SMOOTH = 0.6f

/**
 * 扫码页：相机预览，扫到网页「添加手机」的二维码后交给确认页。
 * 取景框跟着画面里的二维码移动、改变大小（弹簧），框外半透明遮罩；二维码离开画面后回到中间。
 * 扫到的不是 Nagi 的二维码时框变红并提示。扫码成功时框从二维码的位置展开到全屏、填上页面底色，然后进确认页。
 * 「移除动画」时只淡入淡出。
 */
@Composable
fun ScanScreen(onBack: () -> Unit, onLocked: (PairingUri) -> Unit = {}, onScanned: (PairingUri) -> Unit) {
    val context = LocalContext.current
    var granted by remember {
        mutableStateOf(ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED)
    }
    var denied by remember { mutableStateOf(false) }
    val request = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {
        granted = it
        denied = !it
    }
    LaunchedEffect(Unit) { if (!granted) request.launch(Manifest.permission.CAMERA) }

    var seen by remember { mutableStateOf<Seen?>(null) }
    var wrong by remember { mutableStateOf(false) }
    var success by remember { mutableStateOf<PairingUri?>(null) }
    // 这一轮连续跟踪从什么时候开始；解出 Nagi 的二维码但还没跟够 TRACK_MIN_MS 时先记着，跟够了再算成功。
    var trackSince by remember { mutableLongStateOf(0L) }
    var pending by remember { mutableStateOf<PairingUri?>(null) }
    val reduced = isReducedMotion()
    val scanned by rememberUpdatedState(onScanned)
    val locked by rememberUpdatedState(onLocked)
    val view = LocalView.current

    // 二维码离开画面后，过一会儿让框回到中间。
    LaunchedEffect(seen) {
        val s = seen ?: return@LaunchedEffect
        delay(LOST_MS)
        if (seen === s && success == null) {
            seen = null
            wrong = false
            pending = null
        }
    }
    // 先跟着二维码，跟够 TRACK_MIN_MS 才算成功，所以一定能看到框跟上二维码再锁定、展开。
    LaunchedEffect(pending) {
        val p = pending ?: return@LaunchedEffect
        delay((TRACK_MIN_MS - (System.currentTimeMillis() - trackSince)).coerceAtLeast(0))
        if (pending === p && seen != null && success == null) success = p
    }

    BoxWithConstraints(Modifier.fillMaxSize().background(Color.Black)) {
        val density = LocalDensity.current
        val w = with(density) { maxWidth.toPx() }
        val h = with(density) { maxHeight.toPx() }
        val idle = with(density) { IdleFrame.toPx() }
        val idleRect = Rect(Offset((w - idle) / 2, (h - idle) / 2), Size(idle, idle))
        val radiusPx = with(density) { 12.dp.toPx() }

        // 取景框的四条边、圆角和「成功时的填充」都单独做弹簧动画。
        val left = remember { Animatable(idleRect.left) }
        val top = remember { Animatable(idleRect.top) }
        val right = remember { Animatable(idleRect.right) }
        val bottom = remember { Animatable(idleRect.bottom) }
        val radius = remember { Animatable(radiusPx) }
        val fill = remember { Animatable(0f) }
        val red = remember { Animatable(0f) }
        // 锁定二维码时边框加粗：0 平时，1 锁定。
        val lock = remember { Animatable(0f) }

        val target = when {
            success != null -> Rect(0f, 0f, w, h)
            seen != null -> seen!!.rect
            else -> idleRect
        }
        LaunchedEffect(target, success) {
            val s = success
            if (s != null) {
                // 扫码成功分三段，每段都看得清：
                // 1. 对准：框移到二维码的位置、变成它的大小，边框加粗表示锁定。ZXing 解出二维码时才知道它在哪，
                //    而 Nagi 的二维码一解出来就成功了，所以对准放在成功以后做，不然框来不及移过去。
                // 2. 在二维码上停一下。
                // 3. 展开到全屏：前半段只放大、还看得到相机画面，后半段渐变铺上页面底色，然后进确认页。
                val qr = seen?.rect ?: idleRect
                // 一锁定就开始在后台请求配对信息，展开期间数据基本就到了，确认页不用先显示骨架。
                locked(s)
                view.performHapticFeedback(
                    if (Build.VERSION.SDK_INT >= 30) HapticFeedbackConstants.CONFIRM else HapticFeedbackConstants.VIRTUAL_KEY,
                )
                if (reduced) {
                    fill.animateTo(1f, tween(Motion.FADE_IN))
                } else {
                    // 1. 对准并锁定：和跟踪用同一种弹簧，从跟踪自然接上；边框同时加粗。
                    coroutineScope {
                        val aim = spring<Float>(dampingRatio = 0.85f, stiffness = 650f)
                        launch { left.animateTo(qr.left, aim) }
                        launch { top.animateTo(qr.top, aim) }
                        launch { right.animateTo(qr.right, aim) }
                        launch { bottom.animateTo(qr.bottom, aim) }
                        launch { lock.animateTo(1f, tween(180, easing = Motion.Enter)) }
                    }
                    // 2. 停一下。
                    delay(LOCK_HOLD_MS)
                    // 3. 展开到全屏：四边、圆角用同一条曲线同时走完；底色从 FILL_FROM 开始铺，和展开同时结束。
                    coroutineScope {
                        val grow = tween<Float>(EXPAND_MS, easing = Motion.ContentIn)
                        launch { left.animateTo(0f, grow) }
                        launch { top.animateTo(0f, grow) }
                        launch { right.animateTo(w, grow) }
                        launch { bottom.animateTo(h, grow) }
                        launch { radius.animateTo(0f, grow) }
                        val from = (EXPAND_MS * FILL_FROM).toInt()
                        launch { fill.animateTo(1f, tween(EXPAND_MS - from, delayMillis = from, easing = Motion.Enter)) }
                    }
                }
                // 4. 框里已经全是页面底色，进确认页（确认页的内容在同样的底色上淡入）。
                scanned(s)
                return@LaunchedEffect
            }
            if (reduced) {
                left.snapTo(target.left); top.snapTo(target.top); right.snapTo(target.right); bottom.snapTo(target.bottom)
            } else coroutineScope {
                // 跟着二维码：跟得紧、几乎不回弹（位置已经平滑过，回弹会显得飘）。
                val follow = spring<Float>(dampingRatio = 0.85f, stiffness = 650f)
                launch { left.animateTo(target.left, follow) }
                launch { top.animateTo(target.top, follow) }
                launch { right.animateTo(target.right, follow) }
                launch { bottom.animateTo(target.bottom, follow) }
            }
        }
        LaunchedEffect(wrong) { red.animateTo(if (wrong) 1f else 0f, tween(150)) }

        if (granted) {
            CameraPreview(
                onFrame = { text, rect ->
                    if (success != null) return@CameraPreview
                    val now = System.currentTimeMillis()
                    val prev = seen
                    if (prev == null) trackSince = now
                    // 和上一次的位置加权平均；离得远（二维码跳到了别处）就直接用新位置。
                    val r = if (prev != null && (prev.rect.center - rect.center).getDistance() < rect.width) smooth(prev.rect, rect) else rect
                    seen = Seen(text.orEmpty(), r, now)
                    if (text == null) return@CameraPreview
                    val uri = PairingUri.parse(text)
                    if (uri == null) {
                        wrong = true
                        pending = null
                    } else {
                        wrong = false
                        if (now - trackSince >= TRACK_MIN_MS) success = uri else if (pending == null) pending = uri
                    }
                },
            )
            val pageBg = NagiTheme.colors.background
            val white = Color.White
            val danger = NagiTheme.colors.destructive
            Canvas(Modifier.fillMaxSize()) {
                val r = Rect(left.value, top.value, right.value, bottom.value)
                val cr = CornerRadius(radius.value)
                val hole = Path().apply { addRoundRect(RoundRect(r, cr)) }
                // 框外的遮罩：成功时随框展开消失。
                clipPath(hole, ClipOp.Difference) { drawRect(Color.Black.copy(alpha = 0.45f * (1f - fill.value))) }
                // 成功时框里填上页面底色，和确认页接上。
                if (fill.value > 0f) drawRoundRect(pageBg.copy(alpha = fill.value), r.topLeft, r.size, cr)
                // 锁定时边框从 2 加粗到 3。
                val stroke = (2f + lock.value).dp.toPx()
                val frameColor = lerp(white.copy(alpha = 0.9f), danger, red.value).copy(alpha = 0.9f * (1f - fill.value))
                drawRoundRect(frameColor, r.topLeft, r.size, cr, style = Stroke(stroke))
            }
        }
        // 返回键和提示在成功时淡出，不突然消失。
        androidx.compose.animation.AnimatedVisibility(
            visible = success == null,
            enter = fadeIn(tween(Motion.FADE_IN)),
            exit = fadeOut(tween(150, easing = Motion.Exit)),
        ) {
        Box(Modifier.fillMaxSize()) {
            Column(Modifier.align(Alignment.TopStart)) {
                NagiBackTopBar(title = "", onBack = onBack)
            }
            Column(
                Modifier.align(Alignment.BottomCenter).navigationBarsPadding().padding(horizontal = Dimens.pageHorizontal, vertical = 32.dp),
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                val text = when {
                    denied -> stringResource(R.string.scan_no_camera)
                    wrong -> stringResource(R.string.scan_wrong_code)
                    else -> stringResource(R.string.scan_hint)
                }
                Text(text, style = NagiTheme.type.body, color = Color.White, textAlign = TextAlign.Center)
                if (denied) {
                    Spacer(Modifier.height(16.dp))
                    NagiButton(stringResource(R.string.scan_allow_camera), { request.launch(Manifest.permission.CAMERA) }, variant = ButtonVariant.Outline)
                }
            }
        }
        }
    }
}

/**
 * 相机预览、定位和解码。每一帧先只找二维码的定位图形（ZXing 的 Detector，不解内容：离得远、有点糊时常常能找到位置
 * 但解不出内容），找到就把位置换算到预览画面上交给 [onFrame]；再用定位的结果解内容，解出来一起给，解不出 text 为 null。
 * 位置按图像的裁剪范围、旋转角度和预览的铺满方式自己换算（mapToView）。在主线程回调。
 */
@Composable
private fun CameraPreview(onFrame: (String?, Rect) -> Unit) {
    val owner = LocalLifecycleOwner.current
    val executor = remember { Executors.newSingleThreadExecutor() }
    val callback by rememberUpdatedState(onFrame)
    DisposableEffect(Unit) { onDispose { executor.shutdown() } }
    AndroidView(
        modifier = Modifier.fillMaxSize(),
        factory = { ctx ->
            val view = PreviewView(ctx).apply { scaleType = PreviewView.ScaleType.FILL_CENTER }
            var camera: Camera? = null
            var lastFocus = 0L
            var lastFound = 0L
            var preferGlobal = false
            // 对准某一点对焦和测光（预览坐标）。
            fun focusAt(x: Float, y: Float) {
                val control = camera?.cameraControl ?: return
                lastFocus = System.currentTimeMillis()
                val point = view.meteringPointFactory.createPoint(x, y)
                control.startFocusAndMetering(
                    FocusMeteringAction.Builder(point, FocusMeteringAction.FLAG_AF or FocusMeteringAction.FLAG_AE)
                        .setAutoCancelDuration(3, TimeUnit.SECONDS)
                        .build(),
                )
            }
            val future = ProcessCameraProvider.getInstance(ctx)
            future.addListener({
                val provider = future.get()
                // 分析用 16:9、约 1280×720：默认的 640×480 离远一点二维码就糊了，定位和解码都不准。
                // 预览只要求 16:9，分辨率不限，用相机能给的最清楚的；帧率固定 30，光线暗时也不自动降帧。
                val previewResolution = ResolutionSelector.Builder()
                    .setAspectRatioStrategy(AspectRatioStrategy.RATIO_16_9_FALLBACK_AUTO_STRATEGY)
                    .build()
                val resolution = ResolutionSelector.Builder()
                    .setAspectRatioStrategy(AspectRatioStrategy.RATIO_16_9_FALLBACK_AUTO_STRATEGY)
                    .setResolutionStrategy(ResolutionStrategy(android.util.Size(1280, 720), ResolutionStrategy.FALLBACK_RULE_CLOSEST_HIGHER_THEN_LOWER))
                    .build()
                // 预览帧率：这颗摄像头支持 60 就用 60，否则 30。60 和分析图像一起绑定失败时也退回 30（见下面的绑定）。
                fun preview(fps: Int) = Preview.Builder().setResolutionSelector(previewResolution).setTargetFrameRate(Range(fps, fps)).build()
                    .also { it.surfaceProvider = view.surfaceProvider }
                val supports60 = runCatching {
                    provider.getCameraInfo(CameraSelector.DEFAULT_BACK_CAMERA).supportedFrameRateRanges.any { it.upper >= 60 && it.lower <= 60 }
                }.getOrDefault(false)
                val analysis = ImageAnalysis.Builder()
                    .setResolutionSelector(resolution)
                    .setBackpressureStrategy(ImageAnalysis.STRATEGY_KEEP_ONLY_LATEST)
                    .build()
                analysis.setAnalyzer(executor) { image ->
                    val t0 = System.nanoTime()
                    // 两种黑白化：HybridBinarizer 适合一般情况，GlobalHistogramBinarizer 对屏幕摩尔纹、反光更稳。
                    // 先用上一帧成功的那种；没找到或解不出内容时同一帧再用另一种试一次，不用等下一帧。
                    val found = locate(image, preferGlobal)
                    if (found != null) preferGlobal = found.global
                    if (found == null) {
                        // 还没找到二维码：每 CENTER_FOCUS_MS 对画面中心对焦一次，不然没对上焦就一直找不到。
                        val now = System.currentTimeMillis()
                        if (now - lastFocus > CENTER_FOCUS_MS && now - lastFound > CENTER_FOCUS_MS) {
                            ContextCompat.getMainExecutor(ctx).execute { focusAt(view.width / 2f, view.height / 2f) }
                        }
                    } else {
                        lastFound = System.currentTimeMillis()
                    }
                    val ms = (System.nanoTime() - t0) / 1_000_000
                    if (BuildConfig.DEBUG) {
                        Log.d(
                            "NagiScan",
                            "frame ${image.width}x${image.height} rot=${image.imageInfo.rotationDegrees} ${ms}ms " +
                                (found?.let { f -> "pts=" + f.points.joinToString(" ") { "(%.0f,%.0f)".format(it.x, it.y) } + " text=" + (f.text != null) } ?: "none"),
                        )
                    }
                    val frame = if (found != null) FrameGeometry(image.cropRect, image.imageInfo.rotationDegrees) else null
                    image.close()
                    if (found == null || frame == null) return@setAnalyzer
                    ContextCompat.getMainExecutor(ctx).execute {
                        val rect = mapToView(found.points, frame, view.width.toFloat(), view.height.toFloat()) ?: return@execute
                        if (BuildConfig.DEBUG) Log.d("NagiScan", "view ${view.width}x${view.height} rect=$rect")
                        callback(found.text, rect)
                        // 对准二维码对焦和测光，最多每 FOCUS_EVERY_MS 一次，免得来回拉焦。
                        if (System.currentTimeMillis() - lastFocus > FOCUS_EVERY_MS) focusAt(rect.center.x, rect.center.y)
                    }
                }
                // 预览和分析绑在同一个取景范围（ViewPort）上：分析图像的裁剪范围（cropRect）就是预览看得到的那部分，
                // 换算时把它铺满预览即可。
                // 取景范围要等 PreviewView 有了大小才有，所以放到 post 里。
                view.post {
                    val viewPort = view.viewPort ?: return@post
                    fun bind(fps: Int): Camera {
                        val group = UseCaseGroup.Builder().setViewPort(viewPort).addUseCase(preview(fps)).addUseCase(analysis).build()
                        provider.unbindAll()
                        return provider.bindToLifecycle(owner, CameraSelector.DEFAULT_BACK_CAMERA, group)
                    }
                    camera = if (supports60) runCatching { bind(60) }.getOrElse { bind(30) } else bind(30)
                    // 一打开就对画面中心对焦。
                    focusAt(view.width / 2f, view.height / 2f)
                }
            }, ContextCompat.getMainExecutor(ctx))
            view
        },
    )
}

/** 一帧里找到的二维码：定位点（图像坐标）、解出的内容（解不出为 null）和用的是哪种黑白化。 */
private class Found(val points: List<ResultPoint>, val text: String?, val global: Boolean)

// TRY_HARDER：定位器多试几种情况，没找到的帧少很多；每帧本来只要十几毫秒，多花一点值得。
private val hints: Map<DecodeHintType, Any> = mapOf(
    DecodeHintType.POSSIBLE_FORMATS to listOf(BarcodeFormat.QR_CODE),
    DecodeHintType.TRY_HARDER to true,
)

/** 用 Y 平面先定位、再解码。先用 [preferGlobal] 指定的黑白化，没解出内容再换另一种；都解不出时返回找到的位置。 */
private fun locate(image: ImageProxy, preferGlobal: Boolean): Found? {
    val plane = image.planes[0]
    val buf = plane.buffer
    val data = ByteArray(buf.remaining()).also { buf.get(it) }
    val source = PlanarYUVLuminanceSource(data, plane.rowStride, image.height, 0, 0, image.width, image.height, false)
    val first = detect(source, preferGlobal)
    if (first?.text != null) return first
    return detect(source, !preferGlobal)?.takeIf { it.text != null || first == null } ?: first
}

private fun detect(source: PlanarYUVLuminanceSource, global: Boolean): Found? {
    val matrix = try {
        BinaryBitmap(if (global) GlobalHistogramBinarizer(source) else HybridBinarizer(source)).blackMatrix
    } catch (e: Exception) {
        return null
    }
    val detected = try {
        Detector(matrix).detect(hints)
    } catch (e: Exception) {
        return null
    }
    val points = detected.points?.filterNotNull().orEmpty()
    if (points.size < 3) return null
    val text = try {
        Decoder().decode(detected.bits, hints).text
    } catch (e: Exception) {
        null
    }
    return Found(points, text, global)
}

/** 两次定位的加权平均（新位置占 [SMOOTH]）。 */
private fun smooth(old: Rect, new: Rect): Rect {
    fun mix(a: Float, b: Float) = a + (b - a) * SMOOTH
    return Rect(mix(old.left, new.left), mix(old.top, new.top), mix(old.right, new.right), mix(old.bottom, new.bottom))
}

/** 一帧图像的几何信息：裁剪范围（缓冲区坐标）和要顺时针转多少度才是正的。 */
private class FrameGeometry(val crop: android.graphics.Rect, val rotation: Int)

/**
 * 把图像里的定位点换算到预览画面上。定位点在缓冲区坐标里（没旋转）：先减去裁剪范围的左上角，再按 rotation 顺时针转正，
 * 最后按 FILL_CENTER（等比放大到铺满、居中裁掉多出的部分）放到预览里。预览和分析共用取景范围，所以裁剪后的比例和预览一致。
 * 定位图形的中心离二维码边缘约 3.5 个模块，按边长外扩 44% 才是整个二维码；再做成正方形。
 * 以前用 CameraX 的 CoordinateTransform，实测没把 90° 的旋转算进去，框跑到了屏幕外。
 */
private fun mapToView(points: List<ResultPoint>, g: FrameGeometry, viewW: Float, viewH: Float): Rect? {
    if (viewW <= 0f || viewH <= 0f) return null
    val cw = g.crop.width().toFloat()
    val ch = g.crop.height().toFloat()
    val rot = ((g.rotation % 360) + 360) % 360
    val (uw, uh) = if (rot == 90 || rot == 270) ch to cw else cw to ch
    val scale = max(viewW / uw, viewH / uh)
    val dx = (viewW - uw * scale) / 2
    val dy = (viewH - uh * scale) / 2
    val xs = ArrayList<Float>(points.size)
    val ys = ArrayList<Float>(points.size)
    for (p in points) {
        val x = p.x - g.crop.left
        val y = p.y - g.crop.top
        val (ux, uy) = when (rot) {
            90 -> (ch - y) to x
            180 -> (cw - x) to (ch - y)
            270 -> y to (cw - x)
            else -> x to y
        }
        xs += ux * scale + dx
        ys += uy * scale + dy
    }
    val cx = (xs.min() + xs.max()) / 2
    val cy = (ys.min() + ys.max()) / 2
    val side = max(xs.max() - xs.min(), ys.max() - ys.min()) * 1.44f
    if (side <= 0f) return null
    return Rect(Offset(cx - side / 2, cy - side / 2), Size(side, side))
}
