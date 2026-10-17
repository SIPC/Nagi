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
      return create(PortMappingSchema, { published: Number(published), target: Number(target), protocol });
    });
}

export function lines(text: string): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
}
