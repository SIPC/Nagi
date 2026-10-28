import { useState, type ReactNode } from "react";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { Field, Input, Segmented, Textarea } from "@/components/ui";
import { checkPorts, envToText, lines, portsToText, textToEnv, textToPorts } from "@/lib/config";
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
// prefix 加在输入框的 id 前面：同一个页面上可能同时有"添加容器"和"修改容器"两个表单。
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
        <Field label="启动命令" htmlFor={prefix + "command"} hint="留空则使用镜像的默认命令">
          <Input id={prefix + "command"} value={command} onChange={(e) => setCommand(e.target.value)} className="font-mono" />
        </Field>
        {!editing && (
          <Field
            label="对外端口"
            htmlFor={prefix + "ports"}
            hint="格式：节点端口:容器端口，空格分隔"
            error={checkPorts(ports)}
            help="例如 8080:80 53:53/udp。首次部署时转为端口映射，经 Caddy 对外开放；之后在「网络」标签页修改或绑定域名。"
          >
            <Input
              id={prefix + "ports"}
              value={ports}
              onChange={(e) => {
                setPorts(e.target.value);
                // 有问题时阻止提交（浏览器自带的表单校验）。
                e.target.setCustomValidity(checkPorts(e.target.value));
              }}
              className="font-mono"
              placeholder="8080:80"
            />
          </Field>
        )}
        <Field label="工作目录" htmlFor={prefix + "workdir"} help="容器 data/ 目录（上传或从 git 拉取的文件）在容器内的挂载路径。留空则不挂载。">
          <Input id={prefix + "workdir"} value={workdir} onChange={(e) => setWorkdir(e.target.value)} placeholder="/app" className="font-mono" />
        </Field>
        <Field label="持久化目录" htmlFor={prefix + "volumes"} hint="容器内路径，每行一个" help="数据保存在容器目录的 volumes/ 下，包含在快照、备份和迁移中。">
          <Textarea id={prefix + "volumes"} value={volumes} onChange={(e) => setVolumes(e.target.value)} className="font-mono text-[13px]" placeholder="/data" />
        </Field>
        <Field label="环境变量" htmlFor={prefix + "env"} hint="KEY=VALUE，每行一个">
          <Textarea id={prefix + "env"} value={env} onChange={(e) => setEnv(e.target.value)} className="font-mono" />
        </Field>
      </div>
      <details open={advanced} className="group rounded-md border">
        <summary className="cursor-pointer list-none px-3.5 py-2.5 text-[13px] font-medium select-none [&::-webkit-details-marker]:hidden">
          <span className="mr-1.5 inline-block text-muted-foreground transition-transform group-open:rotate-90">›</span>
          资源限制与健康检查
        </summary>
        <div className="grid gap-4 border-t px-3.5 py-4 sm:grid-cols-2">
          <Field label="内存上限（MB）" htmlFor={prefix + "memory"} hint="留空则不限制">
            <Input id={prefix + "memory"} value={memory} onChange={(e) => setMemory(e.target.value)} inputMode="numeric" className="font-mono" />
          </Field>
          <Field label="CPU 上限（核）" htmlFor={prefix + "cpus"} hint="可为小数，如 0.5；留空则不限制">
            <Input id={prefix + "cpus"} value={cpus} onChange={(e) => setCpus(e.target.value)} inputMode="decimal" className="font-mono" />
          </Field>
          <Field label="健康检查命令" htmlFor={prefix + "health"} help="在容器内通过 sh -c 执行，退出码 0 表示健康，连续失败 3 次判定为不健康。">
            <Input id={prefix + "health"} value={health} onChange={(e) => setHealth(e.target.value)} className="font-mono" placeholder="wget -qO- http://localhost/ || exit 1" />
          </Field>
          <Field label="检查间隔（秒）" htmlFor={prefix + "health-interval"} hint="留空则为 30 秒">
            <Input id={prefix + "health-interval"} value={healthInterval} onChange={(e) => setHealthInterval(e.target.value)} inputMode="numeric" className="font-mono" />
          </Field>
        </div>
      </details>
    </div>
  );
  return { fields, build };
}

// 新建项目和添加容器共用的表单：一个容器（镜像表单）或一个容器组（一份 compose）。
// first 表示项目里的第一个容器（没有别的容器可依赖，不显示"依赖"）。
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
      <Field label="类型">
        <div>
          <Segmented
            value={mode}
            onChange={setMode}
            items={[
              { value: "image", label: "容器" },
              { value: "compose", label: "容器组（compose）" },
            ]}
          />
        </div>
      </Field>
      {mode === "compose" ? (
        <Field label="compose" htmlFor="compose" help="compose 文件整体作为一个容器组，适用于自带数据库等依赖的应用。组内容器通过 compose 中的服务名互相访问。">
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
        <Field label="依赖" htmlFor="deps" hint="容器名，逗号分隔" help="批量部署时，优先部署此处列出的容器，例如后端依赖的数据库。">
          <Input id="deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} className="font-mono" placeholder="db" />
        </Field>
      )}
    </>
  );
  return { fields, build, mode };
}
