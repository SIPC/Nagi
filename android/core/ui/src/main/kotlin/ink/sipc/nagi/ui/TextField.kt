package ink.sipc.nagi.ui

import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.collectIsFocusedAsState
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.error
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.unit.dp

/**
 * NagiTextField：单行输入框，和网页的 Input 一样：高 40（手机上比网页的 36 高一点，好点中），圆角 6，1px 边框，
 * 获得焦点时边框变深，出错时变红。[placeholder] 是灰色的提示（例如留空时会用的默认值）。
 */
@Composable
fun NagiTextField(
    value: String,
    onValueChange: (String) -> Unit,
    modifier: Modifier = Modifier,
    label: String? = null,
    placeholder: String? = null,
    error: Boolean = false,
    enabled: Boolean = true,
    maxLength: Int? = null,
    password: Boolean = false,
    keyboardType: KeyboardType = KeyboardType.Text,
    imeAction: ImeAction = ImeAction.Done,
    onDone: () -> Unit = {},
) {
    val c = NagiTheme.colors
    val interaction = remember { MutableInteractionSource() }
    val focused by interaction.collectIsFocusedAsState()
    val borderColor by animateColorAsState(
        when {
            error -> c.destructive
            focused -> c.mutedForeground
            else -> c.border
        },
        tween(150), label = "input",
    )
    val shape = RoundedCornerShape(6.dp)
    BasicTextField(
        value = value,
        onValueChange = { onValueChange(if (maxLength != null) it.take(maxLength) else it) },
        modifier = modifier.fillMaxWidth().semantics {
            if (label != null) contentDescription = label
            if (error) error("")
        },
        enabled = enabled,
        singleLine = true,
        textStyle = NagiTheme.type.body.copy(color = c.foreground),
        cursorBrush = SolidColor(c.foreground),
        keyboardOptions = KeyboardOptions(keyboardType = if (password) KeyboardType.Password else keyboardType, imeAction = imeAction),
        visualTransformation = if (password) PasswordVisualTransformation() else VisualTransformation.None,
        keyboardActions = KeyboardActions(onDone = { onDone() }, onGo = { onDone() }),
        interactionSource = interaction,
        decorationBox = { inner ->
            Box(
                Modifier.heightIn(min = 40.dp).background(c.background, shape).border(1.dp, borderColor, shape).padding(horizontal = 12.dp),
                contentAlignment = Alignment.CenterStart,
            ) {
                if (value.isEmpty() && placeholder != null) Text(placeholder, style = NagiTheme.type.body, color = c.mutedForeground, maxLines = 1)
                inner()
            }
        },
    )
}
