import type { ServiceStatus } from "@/gen/nagi/v1/common_pb";
import { Status } from "@/components/ui";

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
  if (!service.state) return <Status>未部署</Status>;
  if (service.health === "unhealthy") return <Status tone="danger">不健康</Status>;
  if (service.state === "running" && service.health === "starting") return <Status tone="warning">启动中</Status>;
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

// stateOf 是项目（或一个容器组）的整体状态：颜色和文字。
export function stateOf(services: ServiceStatus[], deployed: boolean): { tone: "success" | "warning" | "danger" | "neutral"; label: string } {
  if (!deployed) return { tone: "neutral", label: "未部署" };
  const total = services.length;
  const running = services.filter((s) => s.state === "running").length;
  if (services.some((s) => s.health === "unhealthy" || s.state === "restarting")) return { tone: "danger", label: "异常" };
  if (total > 0 && running === total) return { tone: "success", label: "运行中" };
  if (running > 0) return { tone: "warning", label: `部分运行 ${running}/${total}` };
  return { tone: "neutral", label: "已停止" };
}

// 项目（或一个容器组）的整体状态：全部运行、部分运行、全部停止或未部署。
export function ProjectState({ services, deployed }: { services: ServiceStatus[]; deployed: boolean }) {
  if (!deployed) return <Status>未部署</Status>;
  const total = services.length;
  const running = services.filter((s) => s.state === "running").length;
  const unhealthy = services.some((s) => s.health === "unhealthy" || s.state === "restarting");
  if (unhealthy) return <Status tone="danger">异常</Status>;
  if (total > 0 && running === total) return <Status tone="success">运行中</Status>;
  if (running > 0) return <Status tone="warning">部分运行 {running}/{total}</Status>;
  return <Status>已停止</Status>;
}
