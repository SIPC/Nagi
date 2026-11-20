package ink.sipc.nagi.data

import com.google.protobuf.ByteString
import ink.sipc.nagi.proto.v1.Container
import ink.sipc.nagi.proto.v1.FailoverConfig
import ink.sipc.nagi.proto.v1.FailoverParticipant
import ink.sipc.nagi.proto.v1.MeshPeerStatus
import ink.sipc.nagi.proto.v1.MeshStatus
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.proto.v1.Project
import ink.sipc.nagi.proto.v1.ProjectSummary
import kotlinx.coroutines.flow.flowOf
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import kotlin.math.abs
import kotlin.math.hypot

// 节点网络和高可用的计算（docs/android.md「节点网络」「高可用」，算法同网页的 node-map.tsx）。
class MapDataTest {
    private fun node(id: String, online: Boolean = true, pending: Boolean = false) =
        Node.newBuilder().setId(id).setName("n-$id").setOnline(online).setPending(pending).build()

    private fun key(id: String) = ByteString.copyFromUtf8("key-$id")

    private fun peer(to: String, path: String, rtt: Double = 0.0, tried: Int = 0, heard: Boolean = false) =
        MeshPeerStatus.newBuilder().setPublicKey(key(to)).setName("n-$to").setPath(path).setRttMs(rtt).setTried(tried).setHeard(heard).build()

    private fun status(id: String, vararg peers: MeshPeerStatus, nat: String = "", port: Int = 51820) =
        MeshStatus.newBuilder().setPublicKey(key(id)).setNat(nat).setPort(port).addAllPeers(peers.toList()).build()

    // ---- 布局 ----

    @Test
    fun layoutPutsPanelOnTopAndNodesOnCircle() {
        val l = mapLayout(listOf("a", "b", "c"))
        val panel = l.pos.getValue(MAP_PANEL)
        assertEquals(0f, panel.x, 1e-3f)
        assertEquals(-180f, panel.y, 1e-3f)
        for (id in listOf("a", "b", "c")) {
            val p = l.pos.getValue(id)
            assertEquals(180f, hypot(p.x, p.y), 1e-3f)
        }
        // 四等分：第二个节点在正下方。
        assertEquals(0f, l.pos.getValue("b").x, 1e-3f)
        assertEquals(180f, l.pos.getValue("b").y, 1e-3f)
        assertEquals(2 * (180f + 170f), l.box.w, 1e-3f)
        // 画布左右对称，上下容得下节点和文字。
        assertEquals(-l.box.w / 2, l.box.x, 1e-3f)
        assertTrue(l.box.y <= -180f - 64f)
        assertEquals(180f + 64f, l.box.y + l.box.h, 1e-3f)
    }

    @Test
    fun layoutShrinksToAvailableWidth() {
        val ids = (1..8).map { "n$it" }
        val wide = mapLayout(ids)
        val narrow = mapLayout(ids, 600f)
        assertTrue(narrow.box.w < wide.box.w)
        // 圆最小到相邻节点不相碰：圆心至少隔 72。
        val r = -narrow.pos.getValue(MAP_PANEL).y
        assertTrue(narrow.spacing >= 72f - 1e-3f)
        assertTrue(r >= 120f)
    }

    @Test
    fun labelsSitOutsideTheCircle() {
        assertTrue(mapLabelOnSide(MapPoint(200f, 10f)))
        assertFalse(mapLabelOnSide(MapPoint(10f, 200f)))
    }

    @Test
    fun arrowStopsOutsideTheTarget() {
        val a = MapPoint(0f, 0f)
        val b = MapPoint(100f, 0f)
        val arrow = mapArrow(a, b, MapPoint(50f, 0f))
        assertEquals(69f, arrow.tip.x, 0.1f)
        assertEquals(3, arrow.head.size)
        // 箭头的两个后角在尖的后面 8、两侧各 4。
        assertEquals(61f, arrow.head[1].x, 0.1f)
        assertEquals(4f, abs(arrow.head[1].y), 0.1f)
    }

    @Test
    fun formatsLatencyLikeTheWeb() {
        assertEquals("0.4 ms", formatMs(0.42))
        assertEquals("12 ms", formatMs(12.4))
        assertEquals("1.50 s", formatMs(1500.0))
        assertEquals("12.0 s", formatMs(12_000.0))
        assertEquals("2.0 min", formatMs(120_000.0))
    }

    // ---- 节点网络 ----

    @Test
    fun linksTakeTheWorsePathAndAverageLatency() {
        val nodes = listOf(node("a"), node("b"), node("c"))
        val statuses = mapOf(
            "a" to status("a", peer("b", "direct", 10.0), peer("c", "direct", 20.0)),
            "b" to status("b", peer("a", "direct", 30.0)),
            "c" to status("c", peer("a", "udp-relay")),
        )
        val links = buildMeshLinks(nodes, statuses, null, setOf(MAP_PANEL, "a", "b", "c"))
        val ab = links.first { it.key == "a|b" }
        assertEquals(MeshKind.DIRECT, ab.kind)
        assertEquals(20.0, ab.rtt, 1e-9)
        val ac = links.first { it.key == "a|c" }
        assertEquals("udp-relay", ac.path)
        assertEquals(MeshKind.RELAY, ac.kind)
        assertTrue(ac.viaPanel)
        assertEquals(0.0, ac.rtt, 0.0)
        // 面板和在线节点之间补上控制连接，按直连画，列表里不列。
        val panelLinks = links.filter { it.toPanel }
        assertEquals(3, panelLinks.size)
        assertTrue(panelLinks.all { it.kind == MeshKind.DIRECT && it.views.isEmpty() })
        assertEquals(listOf("a|c", "a|b"), meshListOrder(links).map { it.key })
    }

    @Test
    fun linksIgnoreBadNumbersAndHiddenEnds() {
        val nodes = listOf(node("a"), node("b"), node("x", online = false))
        val statuses = mapOf(
            "a" to status("a", peer("b", "direct", Double.NaN), peer("x", "direct", 5.0)),
            "b" to status("b", peer("a", "direct", -3.0)),
        )
        val links = buildMeshLinks(nodes, statuses, null, setOf(MAP_PANEL, "a", "b"))
        assertTrue(links.none { it.touches("x") })
        assertEquals(0.0, links.first { it.key == "a|b" }.rtt, 0.0)
    }

    @Test
    fun listPutsUnreachableFirstAndSortsDirectByLatency() {
        val nodes = listOf(node("a"), node("b"), node("c"), node("d"))
        val statuses = mapOf(
            "a" to status("a", peer("b", "direct", 40.0), peer("c", "direct", 5.0), peer("d", "node-relay")),
            "b" to status("b", peer("c", "none"), peer("d", "direct")),
        )
        val order = meshListOrder(buildMeshLinks(nodes, statuses, null, setOf(MAP_PANEL, "a", "b", "c", "d"))).map { it.key }
        assertEquals(listOf("b|c", "a|d", "a|c", "a|b", "b|d"), order)
    }

    @Test
    fun relayReasons() {
        fun reason(sa: MeshStatus, sb: MeshStatus): RelayReason? {
            val links = buildMeshLinks(listOf(node("a"), node("b")), mapOf("a" to sa, "b" to sb), null, setOf("a", "b"))
            return relayReason(links.single(), mapOf("a" to sa, "b" to sb))
        }
        assertEquals(
            RelayReason.Symmetric("a", "b", listOf("b")),
            reason(status("a", peer("b", "udp-relay", tried = 1), nat = "nat"), status("b", peer("a", "udp-relay", tried = 1), nat = "symmetric")),
        )
        assertEquals(RelayReason.NoAddress("b"), reason(status("a", peer("b", "udp-relay"), nat = "none"), status("b", peer("a", "udp-relay"))))
        // a 收得到 b 的包，b 收不到 a 的：b 的端口不通。
        assertEquals(
            RelayReason.OneWay("b", "a", 4000),
            reason(status("a", peer("b", "udp-relay", tried = 1, heard = true)), status("b", peer("a", "udp-relay", tried = 1), port = 4000)),
        )
        assertEquals(
            RelayReason.NoReply("a", 51820, "b", 51820),
            reason(status("a", peer("b", "udp-relay", tried = 1)), status("b", peer("a", "udp-relay", tried = 2))),
        )
        // 旧版本不报探测情况：看不出原因。
        assertNull(reason(status("a", peer("b", "udp-relay")), status("b", peer("a", "udp-relay"))))
    }

    @Test
    fun cellsAreDirectional() {
        val cells = meshCells(mapOf("a" to status("a", peer("b", "direct", 12.0)), "b" to status("b", peer("a", "tls-relay", 99.0))))
        assertEquals(MeshCell("direct", 12.0), cells["a|b"])
        assertEquals(MeshCell("tls-relay", 0.0), cells["b|a"])
    }

    @Test
    fun latencySeriesAreAligned() {
        val h = alignLatency(listOf(10L, 20L, 30L) to listOf(1.0, 2.0, 3.0), listOf(20L, 40L) to listOf(5.0, 6.0))
        assertEquals(listOf(10L, 20L, 30L, 40L), h.times)
        assertEquals(listOf(1.0, 2.0, 3.0, 3.0), h.ab)
        assertEquals(listOf(5.0, 5.0, 5.0, 6.0), h.ba)
        // 一端是面板：只有一个方向。
        val one = alignLatency(listOf(1L, 2L) to listOf(3.0, 4.0), emptyList<Long>() to emptyList())
        assertEquals(listOf(1L, 2L), one.times)
        assertTrue(one.ba.isEmpty())
    }

    @Test
    fun keepLastHidesReloads() = runBlocking {
        val ready = Polled.Ready(1)
        val out = flowOf<Polled<Int>>(Polled.Loading, ready, Polled.Loading, Polled.Failed("x", true)).keepLast().toList()
        assertEquals(listOf(Polled.Loading, ready, ready, ready.copy(live = false, error = "x")), out)
    }

    // ---- 高可用 ----

    private fun part(id: String) = FailoverParticipant.newBuilder().setNodeId(id).build()

    private fun container(
        name: String,
        on: String,
        stateful: Boolean = false,
        standby: String? = null,
        witness: String? = null,
        auto: Boolean = false,
        backups: List<String> = emptyList(),
        deployed: Long = 1,
    ): Container {
        val b = Container.newBuilder().setName(name).setNodeId(on).setStateful(stateful).setDeployedVersion(deployed).addAllBackupNodeIds(backups)
        if (standby != null) {
            val f = FailoverConfig.newBuilder().setA(part(on)).setB(part(standby)).setAutoStateful(auto)
            if (witness != null) f.setWitness(part(witness)) else f.setWitnessIsPanel(true)
            b.setFailover(f)
        }
        return b.build()
    }

    private fun items(vararg c: Container) = c.map { HaItem("p", "proj", it) }

    @Test
    fun onlyDeployedContainersCount() {
        val p = ProjectSummary.newBuilder().setProject(
            Project.newBuilder().setId("p").setName("proj")
                .addContainers(container("web", "a")).addContainers(container("draft", "a", deployed = 0)),
        ).build()
        assertEquals(listOf("web"), haItems(listOf(p)).map { it.container.name })
    }

    @Test
    fun fatesFollowTheWebRules() {
        val up = { id: String -> id != "down" }
        assertEquals(HaFate.Auto("b"), haFate(container("x", "a", standby = "b"), up))
        assertEquals(HaFate.Confirm("b"), haFate(container("x", "a", stateful = true, standby = "b"), up))
        assertEquals(HaFate.Auto("b"), haFate(container("x", "a", stateful = true, standby = "b", auto = true), up))
        assertEquals(HaFate.StandbyOffline("down"), haFate(container("x", "a", standby = "down"), up))
        assertEquals(HaFate.WitnessOffline("down"), haFate(container("x", "a", standby = "b", witness = "down"), up))
        // 顶替做不了时退而在备份节点上恢复。
        assertEquals(HaFate.Backup(listOf("c")), haFate(container("x", "a", standby = "down", backups = listOf("down", "c")), up))
        assertEquals(HaFate.BackupOffline(listOf("down")), haFate(container("x", "a", backups = listOf("down")), up))
        assertEquals(HaFate.Lost, haFate(container("x", "a", stateful = true), up))
        assertEquals(HaFate.Redeploy, haFate(container("x", "a"), up))
    }

    @Test
    fun simulationGroupsBySeverity() {
        val nodes = listOf(node("a"), node("b"), node("c", online = false))
        val list = items(
            container("auto", "a", standby = "b"),
            container("lost", "a", stateful = true),
            container("blocked", "a", standby = "c"),
            container("free", "a"),
            container("other", "b", standby = "a", backups = listOf("a")),
            container("elsewhere", "b"),
        )
        val groups = simulateFailure("a", nodes, list)
        assertEquals(listOf(HaGroup.LOST, HaGroup.AUTO, HaGroup.REDEPLOY, HaGroup.COLLATERAL), groups.map { it.first })
        // 组里按网页的顺序：无副本在前，顶替节点离线在后。
        assertEquals(listOf("lost", "blocked"), groups[0].second.map { it.item.container.name })
        assertEquals(HaFate.Collateral("b", failover = true, backup = true), groups[3].second.single().fate)
        assertTrue(simulateFailure("c", nodes, list).single().first == HaGroup.COLLATERAL)
    }

    @Test
    fun edgesCountContainersAndSkipHiddenNodes() {
        val list = items(
            container("x", "a", standby = "b"),
            container("y", "a", standby = "b", backups = listOf("b", "z")),
            container("w", "b", backups = listOf("a")),
            container("s", "a", stateful = true),
        )
        val edges = haEdges(list, setOf("a", "b"))
        assertEquals(
            setOf(HaEdge("a", "b", HaEdgeType.FAILOVER, 2), HaEdge("a", "b", HaEdgeType.BACKUP, 1), HaEdge("b", "a", HaEdgeType.BACKUP, 1)),
            edges.toSet(),
        )
        assertEquals(mapOf("a" to 1), haUnprotected(list))
        // 同一对节点之间的三条线平行错开 10，中间那条在正中。
        val pos = mapOf("a" to MapPoint(0f, 0f), "b" to MapPoint(100f, 0f))
        val shapes = haShapes(edges, pos)
        val ys = edges.map { shapes.getValue(it.key).arrow.from.y }.sorted()
        assertEquals(listOf(-10f, 0f, 10f), ys)
        assertEquals(3, edges.map { shapes.getValue(it.key).label }.toSet().size)
    }
}
