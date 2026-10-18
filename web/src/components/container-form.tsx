import { useState, type ReactNode } from "react";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { Button, Field, Input, Textarea } from "@/components/ui";
import { envToText, lines, portsToText, textToEnv, textToPorts } from "@/lib/config";
import type { ImageForm, ImageFormSchema, ContainerConfigSchema } from "@/gen/nagi/v1/common_pb";

const exampleCompose = `services:
  web:
    image: nginx:alpine
    ports:
      - "8080:80"
`;

export type ContainerConfigInit = MessageInitShape<typeof ContainerConfigSchema>;
export type ImageFormInit = MessageInitShape<typeof ImageFormSchema>;

// useImageForm 是单个容器的镜像表单：新建容器和修改容器共用，所以所有字段建好以后都能改。
// 例外是对外端口：它只在第一次部署时导入成端口映射，之后以"端口映射"为准，所以修改时（传了 initial）不显示。
// prefix 加在输入框的 id 前面：同一个页面上可能同时有"加容器"和"修改容器"两个表单。
export function useImageForm(initial?: ImageForm, prefix = ""): { fields: ReactNode; build: () => ImageFormInit } {
  const editing = !!initial;
  const [image, setImage] = useState(initial?.image ?? "");
  const [command, setCommand] = useState(initial?.command ?? "");
  const [ports, setPorts] = useState(portsToText(initial?.ports));
  const [volumes, setVolumes] = useState((initial?.volumes ?? []).join("\n"));
  const [workdir, setWorkdir] = useState(initial?.workdir ?? "");
  const [env, setEnv] = useState(envToText(initial?.env));
  const [memory, setMemory] = useState(initial?.memoryMb ? String(initial.memoryMb) : "");
  const [cpus, setCpus] = useState(initial?.cpus ? String(initial.cpus) : "");
  const [health, setHealth] = useState(initial?.healthCommand ?? "");
  const [healthInterval, setHealthInterval] = useState(initial?.healthIntervalSeconds ? String(initial.healthIntervalSeconds) : "");
  const advanced = !!(initial?.memoryMb || initial?.cpus || initial?.healthCommand);

  const build = (): ImageFormInit => ({
    image: image.trim(),
    command,
    ports: textToPorts(ports),
    volumes: lines(volumes),
    workdir: workdir.trim(),
    env: textToEnv(env),
    memoryMb: Number(memory) || 0,
    cpus: Number(cpus) || 0,
    healthCommand: health.trim(),
    healthIntervalSeconds: Number(healthInterval) || 0,
  });

  const fields = (
    <div className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="镜像" htmlFor={prefix + "image"}>
          <Input id={prefix + "image"} value={image} onChange={(e) => setImage(e.target.value)} required className="font-mono" placeholder="nginx:alpine" />
        </Field>
        <Field label="运行命令" htmlFor={prefix + "command"} hint="为空时使用镜像默认命令">
          <Input id={prefix + "command"} value={command} onChange={(e) => setCommand(e.target.value)} className="font-mono" />
        </Field>
        {editing ? (
          <div className="grid gap-1.5 text-sm">
            <span className="font-medium">对外端口</span>
            <p className="text-muted-foreground">在下面的"端口映射"里改：映射改了立即生效，不用重新部署，还可以绑域名。</p>
          </div>
        ) : (
          <Field label="对外端口" htmlFor={prefix + "ports"} hint="节点端口:容器端口，空格分隔，例如 8080:80 53:53/udp。部署时变成端口映射，经 Caddy 对外开放；之后在项目页的&quot;端口映射&quot;里改或绑域名">
            <Input id={prefix + "ports"} value={ports} onChange={(e) => setPorts(e.target.value)} className="font-mono" />
          </Field>
        )}
        <Field label="工作目录" htmlFor={prefix + "workdir"} hint="把容器的 data/（上传或从 git 拉的文件）挂到容器内这个路径；为空表示不挂载文件">
          <Input id={prefix + "workdir"} value={workdir} onChange={(e) => setWorkdir(e.target.value)} placeholder="/app" className="font-mono" />
        </Field>
        <Field label="持久化目录" htmlFor={prefix + "volumes"} hint="容器内路径，每行一个；数据保存在容器目录的 volumes/ 下，随快照备份">
          <Textarea id={prefix + "volumes"} value={volumes} onChange={(e) => setVolumes(e.target.value)} className="font-mono" placeholder="/data" />
        </Field>
        <Field label="环境变量" htmlFor={prefix + "env"} hint="KEY=VALUE，每行一个">
          <Textarea id={prefix + "env"} value={env} onChange={(e) => setEnv(e.target.value)} className="font-mono" />
        </Field>
      </div>
      <details open={advanced} className="rounded-md border p-3">
        <summary className="cursor-pointer text-sm font-medium">高级：资源限制、健康检查</summary>
        <div className="mt-3 grid gap-4 sm:grid-cols-2">
          <Field label="内存上限（MB）" htmlFor={prefix + "memory"} hint="为空表示不限制">
            <Input id={prefix + "memory"} value={memory} onChange={(e) => setMemory(e.target.value)} inputMode="numeric" className="font-mono" />
          </Field>
          <Field label="CPU 上限（核）" htmlFor={prefix + "cpus"} hint="可以是小数，例如 0.5；为空表示不限制">
            <Input id={prefix + "cpus"} value={cpus} onChange={(e) => setCpus(e.target.value)} inputMode="decimal" className="font-mono" />
          </Field>
          <Field label="健康检查命令" htmlFor={prefix + "health"} hint="在容器里用 sh -c 执行，退出码 0 表示健康，连续失败 3 次算不健康">
            <Input id={prefix + "health"} value={health} onChange={(e) => setHealth(e.target.value)} className="font-mono" placeholder="wget -qO- http://localhost/ || exit 1" />
          </Field>
          <Field label="检查间隔（秒）" htmlFor={prefix + "health-interval"} hint="为空表示 30 秒">
            <Input id={prefix + "health-interval"} value={healthInterval} onChange={(e) => setHealthInterval(e.target.value)} inputMode="numeric" className="font-mono" />
          </Field>
        </div>
      </details>
    </div>
  );
  return { fields, build };
}

// 新建项目和加容器共用的表单：一个容器（镜像表单）或一个容器组（一份 compose）。
// first 表示项目里的第一个容器（没有别的容器可依赖，不显示"先部署的容器"）。
export function useContainerForm(first = false): { fields: ReactNode; build: () => ContainerConfigInit; mode: "compose" | "image" } {
  const [mode, setMode] = useState<"compose" | "image">("image");
  const [compose, setCompose] = useState(exampleCompose);
  const [dependsOn, setDependsOn] = useState("");
  const imageForm = useImageForm();

  const build = (): ContainerConfigInit => {
    const deps = dependsOn.split(/[\s,]+/).filter(Boolean);
    if (mode === "compose") return { compose, dependsOn: deps };
    return { dependsOn: deps, form: imageForm.build() };
  };

  const fields = (
    <>
      <div className="flex gap-2">
        <Button type="button" size="sm" variant={mode === "image" ? "default" : "outline"} onClick={() => setMode("image")}>
          容器
        </Button>
        <Button type="button" size="sm" variant={mode === "compose" ? "default" : "outline"} onClick={() => setMode("compose")}>
          容器组（compose）
        </Button>
      </div>
      {mode === "compose" ? (
        <Field label="compose" htmlFor="compose" hint="整份作为一个容器组，例如自带数据库的开源应用。组里的容器照常用 compose 里的服务名互相访问。">
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
        imageForm.fields
      )}
      {!first && (
        <Field label="先部署的容器（可选）" htmlFor="deps" hint="一起部署时排在这些容器后面，例如后端依赖数据库。容器名，逗号分隔。">
          <Input id="deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} className="font-mono" placeholder="db" />
        </Field>
      )}
    </>
  );
  return { fields, build, mode };
}
