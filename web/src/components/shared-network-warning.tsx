import { useQuery } from "@tanstack/react-query";
import { nodeClient } from "@/lib/api";
import { Notice } from "@/components/ui";

// SharedNetworkWarning：项目移到目标节点（迁移、顶替、重建）后会断开哪些共享网络。
// 目标节点上没有的网络会退出；有同名网络但没有和原节点相连的，与原节点上的项目断开。
export function SharedNetworkWarning({
  fromNodeId,
  targetNodeId,
  networks,
}: {
  fromNodeId: string;
  targetNodeId: string;
  networks: string[];
}) {
  const list = useQuery({
    queryKey: ["shared-networks", targetNodeId],
    queryFn: () => nodeClient.listNodeSharedNetworks({ nodeId: targetNodeId }),
    enabled: !!targetNodeId && networks.length > 0,
  });
  if (!targetNodeId || networks.length === 0 || !list.data) return null;
  const missing: string[] = [];
  const unlinked: string[] = [];
  for (const n of networks) {
    const t = list.data.networks.find((x) => x.name === n);
    if (!t) missing.push(n);
    else if (!t.peers.some((p) => p.nodeId === fromNodeId)) unlinked.push(n);
  }
  if (missing.length === 0 && unlinked.length === 0) return null;
  return (
    <Notice tone="warning" title="共享网络连接将中断">
      {missing.length > 0 && <div>目标节点上没有共享网络 {missing.join("、")}，项目将退出这些网络。</div>}
      {unlinked.length > 0 && <div>目标节点上的 {unlinked.join("、")} 未与原节点相连，项目将与原节点上的项目断开。可先在节点页连接网络。</div>}
    </Notice>
  );
}
