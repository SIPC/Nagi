import { useState, type ReactNode } from "react";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { Button, Field, Input, Textarea } from "@/components/ui";
import { lines, textToEnv, textToPorts } from "@/lib/config";
import type { MemberConfigSchema } from "@/gen/nagi/v1/common_pb";

const exampleCompose = `services:
  web:
    image: nginx:alpine
    ports:
      - "8080:80"
`;

export type MemberConfigInit = MessageInitShape<typeof MemberConfigSchema>;

// 新建项目和加成员共用的表单：单容器（镜像表单）或一份 compose。
export function useMemberForm(): { fields: ReactNode; build: () => MemberConfigInit; mode: "compose" | "image" } {
  const [mode, setMode] = useState<"compose" | "image">("image");
  const [compose, setCompose] = useState(exampleCompose);
  const [image, setImage] = useState("nginx:alpine");
  const [command, setCommand] = useState("");
  const [ports, setPorts] = useState("");
  const [volumes, setVolumes] = useState("");
  const [workdir, setWorkdir] = useState("");
  const [env, setEnv] = useState("");
  const [dependsOn, setDependsOn] = useState("");

  const build = (): MemberConfigInit => {
    const deps = dependsOn.split(/[\s,]+/).filter(Boolean);
    if (mode === "compose") return { compose, dependsOn: deps };
    return {
      dependsOn: deps,
      form: { image, command, ports: textToPorts(ports), volumes: lines(volumes), workdir, env: textToEnv(env) },
    };
  };

  const fields = (
    <>
      <div className="flex gap-2">
        <Button type="button" size="sm" variant={mode === "image" ? "default" : "outline"} onClick={() => setMode("image")}>
          单容器（镜像表单）
        </Button>
        <Button type="button" size="sm" variant={mode === "compose" ? "default" : "outline"} onClick={() => setMode("compose")}>
          compose
        </Button>
      </div>
      {mode === "compose" ? (
        <Field label="compose" htmlFor="compose" hint="整份作为一个成员，例如自带数据库的开源应用。内部的服务照常用原名互相访问。">
          <Textarea
            id="compose"
            value={compose}
            onChange={(e) => setCompose(e.target.value)}
            spellCheck={false}
            className="min-h-60 font-mono text-[13px] leading-relaxed"
            required
          />
        </Field>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="镜像" htmlFor="image">
            <Input id="image" value={image} onChange={(e) => setImage(e.target.value)} required className="font-mono" />
          </Field>
          <Field label="运行命令" htmlFor="command" hint="为空时使用镜像默认命令">
            <Input id="command" value={command} onChange={(e) => setCommand(e.target.value)} className="font-mono" />
          </Field>
          <Field label="端口" htmlFor="ports" hint="节点端口:容器端口，空格分隔，例如 8080:80 53:53/udp">
            <Input id="ports" value={ports} onChange={(e) => setPorts(e.target.value)} className="font-mono" />
          </Field>
          <Field label="工作目录" htmlFor="workdir" hint="把成员的 data/ 挂到容器内这个路径；为空表示不挂载文件">
            <Input id="workdir" value={workdir} onChange={(e) => setWorkdir(e.target.value)} placeholder="/app" className="font-mono" />
          </Field>
          <Field label="持久化目录" htmlFor="volumes" hint="容器内路径，每行一个">
            <Textarea id="volumes" value={volumes} onChange={(e) => setVolumes(e.target.value)} className="font-mono" placeholder="/data" />
          </Field>
          <Field label="环境变量" htmlFor="env" hint="KEY=VALUE，每行一个">
            <Textarea id="env" value={env} onChange={(e) => setEnv(e.target.value)} className="font-mono" />
          </Field>
        </div>
      )}
      <Field label="先部署的成员（可选）" htmlFor="deps" hint="一起部署时排在这些成员后面，例如后端依赖数据库。成员名，逗号分隔。">
        <Input id="deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} className="font-mono" placeholder="db" />
      </Field>
    </>
  );
  return { fields, build, mode };
}
