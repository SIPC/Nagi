package ink.sipc.nagi.nodes

import androidx.compose.foundation.background
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import ink.sipc.nagi.data.NodesRepo
import ink.sipc.nagi.proto.v1.PanelSettings
import ink.sipc.nagi.ui.ButtonVariant
import ink.sipc.nagi.ui.Dimens
import ink.sipc.nagi.ui.InlineError
import ink.sipc.nagi.ui.NagiBackTopBar
import ink.sipc.nagi.ui.NagiButton
import ink.sipc.nagi.ui.NagiTextField
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SkeletonRows
import kotlinx.coroutines.CancellationException

/**
 * 添加节点（管理员，和网页的「添加节点」对话框一样）：面板对外地址（改了以后生成时先保存，安装命令用的是保存过的地址），
 * 生成安装命令，可以复制或分享（docs/android.md 功能表「节点」）。手机上是整页表单，按钮撑满。
 */
@Composable
fun AddNodeScreen(repo: NodesRepo, onBack: () -> Unit, onDone: () -> Unit) {
    var settings by remember { mutableStateOf<PanelSettings?>(null) }
    var loadError by remember { mutableStateOf<String?>(null) }
    var loads by remember { mutableStateOf(0) }
    LaunchedEffect(loads) {
        loadError = null
        try {
            settings = repo.panelSettings()
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            loadError = e.message.orEmpty()
        }
    }
    var address by remember { mutableStateOf<String?>(null) }
    var command by remember { mutableStateOf("") }
    val action = rememberAction()
    val context = LocalContext.current
    val copy = rememberCopy()
    val shareTitle = stringResource(R.string.install_command)
    Column(Modifier.fillMaxSize().navigationBarsPadding()) {
        NagiBackTopBar(stringResource(R.string.add_node), onBack = onBack)
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = Dimens.pageHorizontal).padding(bottom = 24.dp)) {
            Spacer(Modifier.height(Dimens.topBarToContent))
            Text(stringResource(R.string.add_node_description), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
            Spacer(Modifier.height(16.dp))
            val s = settings
            when {
                s == null && loadError != null -> InlineError(loadError.orEmpty(), stringResource(R.string.retry), { loads++ })
                s == null -> SkeletonRows(2)
                else -> {
                    val value = address ?: s.publicAddress
                    val generate = {
                        action.run("generate") {
                            // 改过地址时先保存：安装命令用的是保存过的地址。
                            val changed = address?.trim()?.takeIf { it != s.publicAddress }
                            command = repo.enrollCommand(changed)
                            if (changed != null) {
                                settings = s.toBuilder().setPublicAddress(changed).build()
                                address = null
                            }
                        }
                    }
                    // 输入框的 label 只给读屏，字段名另写一行（和添加容器的表单一样）。
                    Text(stringResource(R.string.node_public_address), style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground)
                    Spacer(Modifier.height(8.dp))
                    NagiTextField(
                        value, { address = it; action.error = null }, label = stringResource(R.string.node_public_address),
                        error = action.error != null, keyboardType = KeyboardType.Uri, onDone = { generate() },
                    )
                    Spacer(Modifier.height(6.dp))
                    val hint = listOfNotNull(
                        stringResource(R.string.node_public_address_help),
                        if (s.port > 0) stringResource(R.string.port_hint, s.port) else null,
                    ).joinToString(" ")
                    Text(hint, style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
                    ActionError(action)
                    Spacer(Modifier.height(20.dp))
                    if (command.isEmpty()) {
                        NagiButton(stringResource(R.string.generate), { generate() }, Modifier.fillMaxWidth(), loading = action.busy == "generate")
                    } else {
                        Text(stringResource(R.string.install_command), style = NagiTheme.type.bodyMedium, color = NagiTheme.colors.foreground)
                        Spacer(Modifier.height(8.dp))
                        // 命令很长：不换行，横向滚动，长按可以选择。
                        SelectionContainer {
                            Text(
                                command, style = NagiTheme.type.mono, color = NagiTheme.colors.foreground, softWrap = false,
                                modifier = Modifier.fillMaxWidth().background(NagiTheme.colors.muted, RoundedCornerShape(8.dp))
                                    .horizontalScroll(rememberScrollState()).padding(12.dp),
                            )
                        }
                        Spacer(Modifier.height(8.dp))
                        Text(stringResource(R.string.command_hint), style = NagiTheme.type.secondary, color = NagiTheme.colors.mutedForeground)
                        Spacer(Modifier.height(16.dp))
                        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                            NagiButton(stringResource(R.string.copy), { copy(command) }, Modifier.weight(1f), variant = ButtonVariant.Outline)
                            NagiButton(stringResource(R.string.share), { share(context, command, shareTitle) }, Modifier.weight(1f), variant = ButtonVariant.Outline)
                        }
                        Spacer(Modifier.height(12.dp))
                        NagiButton(stringResource(R.string.regenerate), { generate() }, Modifier.fillMaxWidth(), variant = ButtonVariant.Outline, loading = action.busy == "generate")
                        Spacer(Modifier.height(12.dp))
                        NagiButton(stringResource(R.string.finish), onDone, Modifier.fillMaxWidth())
                    }
                }
            }
        }
    }
}
