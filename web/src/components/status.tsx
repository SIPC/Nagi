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

// stateOf 是项目（或一个容器组）的整体状态：颜色和文字。
export function stateOf(services: ServiceStatus[], deployed: boolean): { tone: "success" | "warning" | "danger" | "neutral"; label: string } {
  if (!deployed) return { tone: "neutral", label: s.notDeployed };
  const total = services.length;
  const running = services.filter((v) => v.state === "running").length;
  if (services.some((v) => v.health === "unhealthy" || v.state === "restarting")) return { tone: "danger", label: s.abnormal };
  if (total > 0 && running === total) return { tone: "success", label: s.running };
  if (running > 0) return { tone: "warning", label: s.partial(running, total) };
  return { tone: "neutral", label: s.stopped };
}

// 项目（或一个容器组）的整体状态：全部运行、部分运行、全部停止或未部署。
export function ProjectState({ services, deployed }: { services: ServiceStatus[]; deployed: boolean }) {
  if (!deployed) return <Status>{s.notDeployed}</Status>;
  const total = services.length;
  const running = services.filter((v) => v.state === "running").length;
  const unhealthy = services.some((v) => v.health === "unhealthy" || v.state === "restarting");
  if (unhealthy) return <Status tone="danger">{s.abnormal}</Status>;
  if (total > 0 && running === total) return <Status tone="success">{s.running}</Status>;
  if (running > 0) return <Status tone="warning">{s.partial(running, total)}</Status>;
  return <Status>{s.stopped}</Status>;
}
