import type { ServiceStatus } from "@/gen/nagi/v1/common_pb";
import { Status } from "@/components/ui";
import { t } from "@/i18n";

const s = t.common.status;

const stateLabels: Record<string, string> = {
  running: s.running,
  exited: s.stopped,
  restarting: s.restarting,
  paused: s.paused,
  created: s.created,
  dead: s.abnormal,
  removing: s.removing,
};

export function ServiceState({ service }: { service: ServiceStatus }) {
  if (!service.state) return <Status>{s.notDeployed}</Status>;
  if (service.health === "unhealthy") return <Status tone="danger">{s.unhealthy}</Status>;
  if (service.state === "running" && service.health === "starting") return <Status tone="warning">{s.starting}</Status>;
  const tone =
    service.state === "running"
      ? "success"
      : service.state === "restarting" || service.state === "paused"
        ? "warning"
        : service.state === "dead"
          ? "danger"
          : "neutral";
  return <Status tone={tone}>{stateLabels[service.state] ?? service.state}</Status>;
}

// offlineNodeNames 列出部署过容器的离线节点。这些节点上的容器状态未知（节点离线不等于容器停止）。
export function offlineNodeNames(containers: { nodeId: string; deployedVersion: bigint }[], nodes?: { id: string; name: string; online: boolean; pending: boolean }[]): string[] {
  const names = new Set<string>();
  for (const c of containers) {
    const n = nodes?.find((x) => x.id === c.nodeId);
    if (n && !n.online && !n.pending && c.deployedVersion > 0n) names.add(n.name);
  }
  return [...names];
}

// serviceDetail 是容器的运行时长等说明。健康状态已由状态列按规则判断，这里去掉 Docker 文字里的健康状态，免得两处矛盾。
export function serviceDetail(status: string): string {
  return status.replace(/\s*\((healthy|unhealthy|health: starting)\)$/, "");
}

// stateOf 是项目（或一个容器组）的整体状态：颜色和文字。offline 是其中离线的节点。
export function stateOf(services: ServiceStatus[], deployed: boolean, offline: string[] = []): { tone: "success" | "warning" | "danger" | "neutral"; label: string } {
  if (!deployed) return { tone: "neutral", label: s.notDeployed };
  const total = services.length;
  const running = services.filter((v) => v.state === "running").length;
  if (services.some((v) => v.health === "unhealthy")) return { tone: "danger", label: s.unhealthy };
  if (services.some((v) => v.state === "dead")) return { tone: "danger", label: s.abnormal };
  if (offline.length > 0) return { tone: "warning", label: s.nodesOffline(offline) };
  if (services.some((v) => v.state === "restarting")) return { tone: "warning", label: s.restarting };
  if (total > 0 && services.every((v) => v.state === "paused")) return { tone: "warning", label: s.paused };
  if (total > 0 && running === total) return services.some((v) => v.health === "starting") ? { tone: "warning", label: s.starting } : { tone: "success", label: s.running };
  if (running > 0) return { tone: "warning", label: s.partial(running, total) };
  return { tone: "neutral", label: s.stopped };
}

// 项目（或一个容器组）的整体状态：全部运行、部分运行、全部停止、未部署或节点离线。
export function ProjectState({ services, deployed, offline }: { services: ServiceStatus[]; deployed: boolean; offline?: string[] }) {
  const st = stateOf(services, deployed, offline);
  return <Status tone={st.tone}>{st.label}</Status>;
}
