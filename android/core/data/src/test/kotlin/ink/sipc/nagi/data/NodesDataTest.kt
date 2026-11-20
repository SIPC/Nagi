package ink.sipc.nagi.data

import ink.sipc.nagi.proto.v1.DiskUsage
import ink.sipc.nagi.proto.v1.Node
import ink.sipc.nagi.proto.v1.NodeInfo
import ink.sipc.nagi.proto.v1.NodeUsage
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

// 节点列表的占用（docs/android.md「节点列表」，算法同网页的 nodeUsage）。
class NodesDataTest {
    private fun node(name: String, online: Boolean = true, pending: Boolean = false, cpu: Double = 10.0, mem: Double = 20.0, disk: Double = 30.0, cpus: Int = 4) =
        Node.newBuilder().setId(name).setName(name).setOnline(online).setPending(pending)
            .setInfo(NodeInfo.newBuilder().setCpus(cpus).setMemoryBytes(8L shl 30).setDiskBytes(100L shl 30))
            .setUsage(NodeUsage.newBuilder().setCpu(cpu).setMemory(mem).setDisk(disk))
            .build()

    @Test
    fun cpuIsFoldedByCores() {
        val u = node("a", cpu = 200.0, cpus = 4).usageInfo()!!
        assertEquals(0.5, u.cpu, 1e-9)
        assertEquals(200.0, u.cpuPercent, 1e-9)
        // 不报核数的旧节点按 100 算。
        assertEquals(1.0, node("b", cpu = 150.0, cpus = 0).usageInfo()!!.cpu, 1e-9)
    }

    @Test
    fun diskSumsAllDisks() {
        val n = node("a").toBuilder().setUsage(
            NodeUsage.newBuilder().setCpu(1.0).setMemory(1.0).setDisk(99.0)
                .addDisks(DiskUsage.newBuilder().setMount("/").setTotalBytes(100).setUsedBytes(25))
                .addDisks(DiskUsage.newBuilder().setMount("/data").setTotalBytes(300).setUsedBytes(75)),
        ).build()
        val u = n.usageInfo()!!
        assertEquals(0.25, u.disk, 1e-9)
        assertEquals(400.0, u.diskTotal, 1e-9)
        assertEquals(100.0, u.diskUsed, 1e-9)
    }

    @Test
    fun badNumbersFromNodeAreIgnored() {
        val u = node("a", cpu = Double.NaN, mem = Double.POSITIVE_INFINITY, disk = -5.0).usageInfo()!!
        assertEquals(0.0, u.cpu, 0.0)
        assertEquals(0.0, u.memory, 0.0)
        assertEquals(0.0, u.disk, 0.0)
        // 很大的数截成上限后再按 100% 封顶。
        assertEquals(1.0, node("b", mem = 1e300).usageInfo()!!.memory, 0.0)
    }

    @Test
    fun offlineAndPendingHaveNoUsage() {
        assertNull(node("a", online = false).usageInfo())
        assertNull(node("b", pending = true).usageInfo())
    }
}
