package ink.sipc.nagi.settings

import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.ui.res.stringResource
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.GetPanelMeshRequest
import ink.sipc.nagi.proto.v1.MeshPeerStatus
import ink.sipc.nagi.ui.ActionRow
import ink.sipc.nagi.ui.NagiTheme
import ink.sipc.nagi.ui.SettingsGroup
import ink.sipc.nagi.ui.StatusDot
import ink.sipc.nagi.ui.formatBytesText
import kotlinx.coroutines.delay

// 节点网络（管理员，和网页设置页的「节点网络」一样，见 pages/mesh.tsx 的 PanelMeshCard）：面板自己在节点网络里的连接，
// 每 5 秒刷新。每个成员一行：名称、连接方式、延迟、最近握手、收发的量；直连时写对方地址、纠错、QUIC 和丢包。
// Ping 和测速在节点页里对节点做。

@Composable
private fun pathLabel(path: String) = stringResource(
    when (path) {
        "direct" -> R.string.mesh_path_direct
        "link" -> R.string.mesh_path_link
        "node-relay" -> R.string.mesh_path_node_relay
        "udp-relay" -> R.string.mesh_path_udp_relay
        "tls-relay" -> R.string.mesh_path_tls_relay
        else -> R.string.mesh_path_none
    },
)

private fun percent(ppm: Int) = if (ppm < 100) "%.2f%%".format(ppm / 10_000.0) else "%.1f%%".format(ppm / 10_000.0)

@Composable
fun MeshScreen(session: PanelSession, onBack: () -> Unit) {
    val mesh = rememberLoad { session.rpc { c, h -> c.nodes.getPanelMesh(GetPanelMeshRequest.getDefaultInstance(), h) } }
    LaunchedEffect(Unit) {
        while (true) {
            delay(5_000)
            mesh.reload()
        }
    }
    SubPage(stringResource(R.string.mesh), onBack) {
        Loaded(mesh) { m ->
            val summary = listOfNotNull(m.address.ifEmpty { null }, m.endpointsList.joinToString("、").ifEmpty { null }).joinToString(" · ")
            if (summary.isNotEmpty()) Note(stringResource(R.string.mesh_endpoints) + " " + summary)
            SettingsGroup(stringResource(R.string.mesh_peers)) {
                if (m.peersCount == 0) Note(stringResource(R.string.mesh_no_peers))
                m.peersList.sortedBy { it.name }.forEach { p -> Peer(p) }
            }
        }
    }
}

@Composable
private fun Peer(p: MeshPeerStatus) {
    val c = NagiTheme.colors
    val parts = buildList {
        add(pathLabel(p.path))
        if (p.path == "direct" && p.endpoint.isNotEmpty()) add(p.endpoint)
        if (p.rttMs > 0) add("%.0f ms".format(p.rttMs))
        add(
            if (p.hasLastHandshake() && p.lastHandshake.seconds > 0) stringResource(R.string.mesh_handshake, ago(p.lastHandshake.seconds))
            else stringResource(R.string.mesh_no_handshake),
        )
        add("↓ ${formatBytesText(p.rxBytes.toDouble())} ↑ ${formatBytesText(p.txBytes.toDouble())}")
        if (p.fec) add(stringResource(R.string.mesh_fec, p.fecParity))
        if (p.quic) add("QUIC")
        if (p.txLossKnown || p.rxLossKnown) {
            add(stringResource(R.string.mesh_loss, listOfNotNull(
                if (p.txLossKnown) "↑ ${percent(p.txLossPpm)}" else null,
                if (p.rxLossKnown) "↓ ${percent(p.rxLossPpm)}" else null,
            ).joinToString(" ")))
        }
    }
    ActionRow(p.name.ifEmpty { p.address }, description = parts.joinToString(" · ")) {
        StatusDot(
            when (p.path) {
                "direct", "link" -> c.success
                "none", "" -> c.destructive
                else -> c.warning
            },
        )
    }
}
