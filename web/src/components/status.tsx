import type { ServiceStatus } from "@/gen/nagi/v1/common_pb";
import { Badge } from "@/components/ui";

const stateLabels: Record<string, string> = {
  running: "运行中",
  exited: "已停止",
  restarting: "重启中",
  paused: "已暂停",
  created: "已创建",
  dead: "异常",
  removing: "删除中",
};

export function ServiceState({ service }: { service: ServiceStatus }) {
  if (!service.state) return <Badge>未部署</Badge>;
  if (service.health === "unhealthy") return <Badge tone="danger" dot>不健康</Badge>;
  if (service.state === "running" && service.health === "starting") {
    return <Badge tone="warning" dot>启动中</Badge>;
  }
  const tone =
    service.state === "running"
      ? "success"
      : service.state === "restarting" || service.state === "paused"
        ? "warning"
        : service.state === "dead"
          ? "danger"
          : "neutral";
  return (
    <Badge tone={tone} dot>
      {stateLabels[service.state] ?? service.state}
    </Badge>
  );
}

// 项目整体状态：全部运行、部分运行、全部停止或未部署。
export function ProjectState({ services, deployed }: { services: ServiceStatus[]; deployed: boolean }) {
  if (!deployed) return <Badge>未部署</Badge>;
  const total = services.length;
  const running = services.filter((s) => s.state === "running").length;
  const unhealthy = services.some((s) => s.health === "unhealthy" || s.state === "restarting");
  if (unhealthy) return <Badge tone="danger" dot>异常</Badge>;
  if (total > 0 && running === total) return <Badge tone="success" dot>运行中</Badge>;
  if (running > 0) return <Badge tone="warning" dot>部分运行 {running}/{total}</Badge>;
  return <Badge dot>已停止</Badge>;
}
