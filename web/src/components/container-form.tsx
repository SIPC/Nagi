import { useState, type ReactNode } from "react";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { Field, Input, Segmented, Textarea } from "@/components/ui";
import { checkPorts, envToText, lines, portsToText, textToEnv, textToPorts } from "@/lib/config";
import { t } from "@/i18n";
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
        <Field label={t.containerForm.image} htmlFor={prefix + "image"}>
          <Input id={prefix + "image"} value={image} onChange={(e) => setImage(e.target.value)} required className="font-mono" placeholder="nginx:alpine" />
        </Field>
        <Field label={t.containerForm.command} htmlFor={prefix + "command"} hint={t.containerForm.commandHint}>
          <Input id={prefix + "command"} value={command} onChange={(e) => setCommand(e.target.value)} className="font-mono" />
        </Field>
        {!editing && (
          <Field
            label={t.containerForm.ports}
            htmlFor={prefix + "ports"}
            hint={t.containerForm.portsHint}
            error={checkPorts(ports)}
            help={t.containerForm.portsHelp}
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
        <Field label={t.containerForm.workdir} htmlFor={prefix + "workdir"} help={t.containerForm.workdirHelp}>
          <Input id={prefix + "workdir"} value={workdir} onChange={(e) => setWorkdir(e.target.value)} placeholder="/app" className="font-mono" />
        </Field>
        <Field label={t.containerForm.volumes} htmlFor={prefix + "volumes"} hint={t.containerForm.volumesHint} help={t.containerForm.volumesHelp}>
          <Textarea id={prefix + "volumes"} value={volumes} onChange={(e) => setVolumes(e.target.value)} className="font-mono text-[13px]" placeholder="/data" />
        </Field>
        <Field label={t.containerForm.env} htmlFor={prefix + "env"} hint={t.containerForm.envHint}>
          <Textarea id={prefix + "env"} value={env} onChange={(e) => setEnv(e.target.value)} className="font-mono" />
        </Field>
      </div>
      <details open={advanced} className="group rounded-md border">
        <summary className="cursor-pointer list-none px-3.5 py-2.5 text-[13px] font-medium select-none [&::-webkit-details-marker]:hidden">
          <span className="mr-1.5 inline-block text-muted-foreground transition-transform group-open:rotate-90">›</span>
          {t.containerForm.advanced}
        </summary>
        <div className="grid gap-4 border-t px-3.5 py-4 sm:grid-cols-2">
          <Field label={t.containerForm.memory} htmlFor={prefix + "memory"} hint={t.containerForm.noLimitHint}>
            <Input id={prefix + "memory"} value={memory} onChange={(e) => setMemory(e.target.value)} inputMode="numeric" className="font-mono" />
          </Field>
          <Field label={t.containerForm.cpus} htmlFor={prefix + "cpus"} hint={t.containerForm.cpusHint}>
            <Input id={prefix + "cpus"} value={cpus} onChange={(e) => setCpus(e.target.value)} inputMode="decimal" className="font-mono" />
          </Field>
          <Field label={t.containerForm.health} htmlFor={prefix + "health"} help={t.containerForm.healthHelp}>
            <Input id={prefix + "health"} value={health} onChange={(e) => setHealth(e.target.value)} className="font-mono" placeholder="wget -qO- http://localhost/ || exit 1" />
          </Field>
          <Field label={t.containerForm.healthInterval} htmlFor={prefix + "health-interval"} hint={t.containerForm.healthIntervalHint}>
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
      <Field label={t.containerForm.type}>
        <div>
          <Segmented
            value={mode}
            onChange={setMode}
            items={[
              { value: "image", label: t.containerForm.typeImage },
              { value: "compose", label: t.containerForm.typeCompose },
            ]}
          />
        </div>
      </Field>
      {mode === "compose" ? (
        <Field label="compose" htmlFor="compose" help={t.containerForm.composeHelp}>
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
        <Field label={t.containerForm.dependsOn} htmlFor="deps" hint={t.containerForm.dependsOnHint} help={t.containerForm.dependsOnHelp}>
          <Input id="deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} className="font-mono" placeholder="db" />
        </Field>
      )}
    </>
  );
  return { fields, build, mode };
}
