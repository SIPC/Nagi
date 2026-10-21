// 测试界面用的简单转换：环境变量用 KEY=VALUE 每行一个的文本编辑。
import { create } from "@bufbuild/protobuf";
import { EnvVarSchema, PortMappingSchema, type EnvVar, type PortMapping } from "@/gen/nagi/v1/common_pb";

export function envToText(vars: EnvVar[] | undefined): string {
  return (vars ?? []).map((v) => `${v.key}=${v.value}`).join("\n");
}

export function textToEnv(text: string): EnvVar[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith("#"))
    .map((l) => {
      const i = l.indexOf("=");
      return create(EnvVarSchema, i < 0 ? { key: l, value: "" } : { key: l.slice(0, i).trim(), value: l.slice(i + 1) });
    });
}

// "8080:80"、"53:53/udp"，用空格或逗号分隔。
export function textToPorts(text: string): PortMapping[] {
  return text
    .split(/[\s,]+/)
    .filter(Boolean)
    .map((p) => {
      const [ports, protocol = "tcp"] = p.split("/");
      const [published, target = published] = ports.split(":");
      return create(PortMappingSchema, { published: Number(published), target: Number(target), protocol: protocol.toLowerCase() });
    });
}

// checkPorts 检查"对外端口"的写法，返回第一个问题的说明，没有问题时返回空。和面板的规则一致：
// 80、443 留给域名访问（网站部署后在「网络」里绑定域名）。
export function checkPorts(text: string): string {
  for (const p of text.split(/[\s,]+/).filter(Boolean)) {
    const m = /^(\d+)(?::(\d+))?(?:\/(tcp|udp))?$/i.exec(p);
    if (!m) return `「${p}」格式不对，应为 节点端口:容器端口，例如 8080:80 或 53:53/udp`;
    const published = Number(m[1]);
    const target = Number(m[2] ?? m[1]);
    if (published < 1 || published > 65535 || target < 1 || target > 65535) return `「${p}」端口应在 1～65535 之间`;
    if (published === 80 || published === 443) return `节点端口 ${published} 留给域名访问。网站请换一个端口（如 8080:${target}），或部署后在「网络」里绑定域名`;
  }
  return "";
}

export function lines(text: string): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
}

export function portsToText(ports: PortMapping[] | undefined): string {
  return (ports ?? [])
    .map((p) => `${p.published}:${p.target}${p.protocol && p.protocol !== "tcp" ? "/" + p.protocol : ""}`)
    .join(" ");
}
