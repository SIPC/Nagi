import { useState, type ReactNode } from "react";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { ChevronRight } from "lucide-react";
import { Field, FormRow, Input, Segmented, Slide, Textarea } from "@/components/ui";
import { cn } from "@/lib/utils";
import { checkEnv, checkPorts, envToText, lines, portsToText, textToEnv, textToPorts } from "@/lib/config";
import { t } from "@/i18n";
import type { ImageForm, ImageFormSchema, ContainerConfigSchema } from "@/gen/nagi/v1/common_pb";
import { EnvEditor } from "@/components/env-editor";

const monoInput = "font-mono text-[13px] max-md:text-base";


const exampleCompose = `services:
  web:
    image: nginx:alpine
    ports:
      - "8080:80"
`;

export type ContainerConfigInit = MessageInitShape<typeof ContainerConfigSchema>;
export type ImageFormInit = MessageInitShape<typeof ImageFormSchema>;

// MoreSettings：折叠的「更多设置」，没有框。有值时默认展开。
function MoreSettings({ label, labelOpen, labelPhone, defaultOpen, children }: { label: string; labelOpen: string; labelPhone?: string; defaultOpen?: boolean; children: ReactNode }) {
  const [open, setOpen] = useState(!!defaultOpen);
  return (
    <>
      <button type="button" aria-expanded={open} onClick={() => setOpen(!open)} className="-ml-1 inline-flex cursor-pointer items-start gap-1 justify-self-start rounded-sm text-left text-muted-foreground outline-none hover:text-foreground focus-visible:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40">
        <ChevronRight className={cn("mt-0.5 size-4 shrink-0 transition-transform", open && "rotate-90")} />
        {open ? labelOpen : <span className={cn(labelPhone && "max-md:hidden")}>{label}</span>}
        {!open && labelPhone && <span className="md:hidden">{labelPhone}</span>}
      </button>
      {open && <div className="grid animate-slide-from-above gap-6 max-md:gap-4">{children}</div>}
    </>
  );
}

// useImageForm 是单个容器的镜像表单：新建容器和修改容器共用，所以所有字段建好以后都能改。
// 例外是对外端口：它只在第一次部署时导入成端口映射，之后以"端口映射"为准，所以修改时（传了 initial）不显示。
// prefix 加在输入框的 id 前面：同一个页面上可能同时有"添加容器"和"修改容器"两个表单。
// top：镜像下面的一行（名称和节点）；moreTop：更多设置开头的字段（容器目录、依赖）。常用的字段直接显示，其余收进更多设置。
// parts：配置页把字段分进几个区块，单独取用；添加容器用排好的 fields。
// hasDeps：更多设置里有没有「依赖」（项目里第一个容器没有），折叠时的标签按它列出字段。
export function useImageForm(initial?: ImageForm, prefix = "", top?: ReactNode, moreTop?: ReactNode, hasDeps = false) {
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
  const volumesField = (
    <Field label={t.containerForm.volumes} htmlFor={prefix + "volumes"} help={`${t.containerForm.volumesHint}${t.common.sentenceEnd}${t.containerForm.volumesHelp}`}>
      <Textarea id={prefix + "volumes"} rows={Math.max(1, volumes.split("\n").length)} value={volumes} onChange={(e) => setVolumes(e.target.value)} className={cn("min-h-8 resize-none py-[5px] max-md:py-[9px]", editing && monoInput)} placeholder="/data" spellCheck={false} />
    </Field>
  );
  const imageField = (
    <Field label={t.containerForm.image} htmlFor={prefix + "image"}>
      <Input id={prefix + "image"} value={image} onChange={(e) => setImage(e.target.value)} required placeholder={t.containerForm.imagePlaceholder} className={cn(editing && monoInput)} autoComplete="off" spellCheck={false} />
    </Field>
  );
  const commandField = (
    <Field label={t.containerForm.command} htmlFor={prefix + "command"}>
      <Input id={prefix + "command"} value={command} onChange={(e) => setCommand(e.target.value)} placeholder={t.containerForm.commandHint} className={cn(editing && monoInput)} autoComplete="off" spellCheck={false} />
    </Field>
  );
  const workdirField = (
    <Field label={t.containerForm.workdir} htmlFor={prefix + "workdir"} help={t.containerForm.workdirHelp}>
      <Input id={prefix + "workdir"} value={workdir} onChange={(e) => setWorkdir(e.target.value)} placeholder="/app" className={cn(editing && monoInput)} autoComplete="off" spellCheck={false} />
    </Field>
  );
  const limits = (
    <>
      <Field label={t.containerForm.memory} htmlFor={prefix + "memory"}>
        <Input id={prefix + "memory"} value={memory} onChange={(e) => setMemory(e.target.value)} inputMode="numeric" placeholder={t.containerForm.noLimitHint} autoComplete="off" />
      </Field>
      <Field label={t.containerForm.cpus} htmlFor={prefix + "cpus"}>
        <Input id={prefix + "cpus"} value={cpus} onChange={(e) => setCpus(e.target.value)} inputMode="decimal" placeholder={t.containerForm.cpusHint} autoComplete="off" />
      </Field>
      <Field label={t.containerForm.health} htmlFor={prefix + "health"} help={t.containerForm.healthHelp}>
        <Input id={prefix + "health"} value={health} onChange={(e) => setHealth(e.target.value)} placeholder="wget -qO- http://localhost/ || exit 1" className={cn(editing && monoInput)} autoComplete="off" spellCheck={false} />
      </Field>
      <Field label={t.containerForm.healthInterval} htmlFor={prefix + "health-interval"}>
        <Input id={prefix + "health-interval"} value={healthInterval} onChange={(e) => setHealthInterval(e.target.value)} inputMode="numeric" placeholder={t.containerForm.healthIntervalHint} autoComplete="off" />
      </Field>
    </>
  );
  const hasMore = !!(initial?.command || initial?.workdir || initial?.memoryMb || initial?.cpus || initial?.healthCommand);

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
    <>
      {imageField}
      {top}
      <FormRow className="gap-y-6 max-md:gap-y-4">
        {!editing && (
          <Field label={t.containerForm.ports} htmlFor={prefix + "ports"} error={checkPorts(ports)} hint={t.containerForm.portsHint} help={t.containerForm.portsHelp}>
            <Input
              id={prefix + "ports"}
              value={ports}
              onChange={(e) => {
                setPorts(e.target.value);
                // 有问题时阻止提交（浏览器自带的表单校验）。
                e.target.setCustomValidity(checkPorts(e.target.value));
              }}
              placeholder="8080:80"
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
        )}
        {volumesField}
      </FormRow>
      <Field label={t.containerForm.env} htmlFor={prefix + "env"} error={checkEnv(env)}>
        <EnvEditor id={prefix + "env"} value={env} onChange={setEnv} />
      </Field>
      <MoreSettings label={t.containerForm.moreImage(hasDeps)} labelOpen={t.containerForm.more} labelPhone={t.containerForm.more} defaultOpen={hasMore}>
        <FormRow className="gap-y-6 max-md:gap-y-4">
          {commandField}
          {workdirField}
          {moreTop}
          {limits}
        </FormRow>
      </MoreSettings>
    </>
  );
  const parts = { image: imageField, command: commandField, volumes: volumesField, workdir: workdirField, limits };
  return { fields, build, parts, env, setEnv };
}

// composeInputClass：compose 编辑框的样式（添加容器组和配置页共用）。
export const composeInputClass = "h-80 resize-y border-0 bg-muted p-3 font-mono text-[13px] leading-5 max-md:h-70 max-md:text-base max-md:leading-6";

// formToCompose：把镜像表单写成 compose 的样子，只用来预览（真正的转换在节点上，见 internal/pkg/spec）。环境变量不写进 compose。
export function formToCompose(service: string, f: ImageFormInit) {
  const out = ["services:", `  ${service}:`, `    image: ${f.image ?? ""}`];
  if (f.command) out.push(`    command: ${f.command}`);
  if (f.workdir) out.push(`    working_dir: ${f.workdir}`);
  const mounts = [...(f.volumes ?? []).map((v) => `${v.split("/").filter(Boolean).join("-") || "data"}:${v}`), ...(f.workdir ? [`./:${f.workdir}`] : [])];
  if (mounts.length > 0) out.push("    volumes:", ...mounts.map((m) => `      - ${m}`));
  if (f.memoryMb) out.push(`    mem_limit: ${f.memoryMb}m`);
  if (f.cpus) out.push(`    cpus: ${f.cpus}`);
  if (f.healthCommand) out.push("    healthcheck:", `      test: ["CMD-SHELL", ${JSON.stringify(f.healthCommand)}]`, ...(f.healthIntervalSeconds ? [`      interval: ${f.healthIntervalSeconds}s`] : []));
  return out.join("\n");
}

// 添加容器的表单：一个容器（镜像表单）或一个容器组（一份 compose）。
// first 表示项目里的第一个容器（没有别的容器可依赖，不显示"依赖"）。
// top 和 moreTop 由调用方给：名称和节点、容器目录。
// dir 时多一个类型「现有目录」（只给管理员）：选中时这里只显示类型切换，表单由调用方画。
export function useContainerForm(
  first = false,
  top?: (mode: "compose" | "image") => ReactNode,
  moreTop?: ReactNode,
  dir = false,
): { fields: ReactNode; build: () => ContainerConfigInit; mode: "compose" | "image" | "dir"; image: () => string } {
  const [mode, setMode] = useState<"compose" | "image" | "dir">("image");
  const [compose, setCompose] = useState(exampleCompose);
  const [dependsOn, setDependsOn] = useState("");
  const deps = !first && (
    <Field label={t.containerForm.dependsOn} htmlFor="deps" help={`${t.containerForm.dependsOnHint}${t.common.sentenceEnd}${t.containerForm.dependsOnHelp}`}>
      <Input id="deps" value={dependsOn} onChange={(e) => setDependsOn(e.target.value)} placeholder="db, cache" autoComplete="off" spellCheck={false} />
    </Field>
  );
  const imageForm = useImageForm(
    undefined,
    "",
    top?.("image"),
    <>
      {moreTop}
      {deps}
    </>,
    !first,
  );

  const build = (): ContainerConfigInit => {
    const list = dependsOn.split(/[\s,]+/).filter(Boolean);
    if (mode === "compose") return { compose, dependsOn: list };
    return { dependsOn: list, form: imageForm.build() };
  };

  const fields = (
    <>
      <div className="max-md:[&>*]:w-full">
        <Segmented
          value={mode}
          onChange={setMode}
          items={[
            { value: "image", label: t.containerForm.typeImage },
            { value: "compose", label: t.containerForm.typeCompose },
            ...(dir ? [{ value: "dir" as const, label: t.containerForm.typeDir }] : []),
          ]}
        />
      </div>
      {mode !== "dir" && (
      <Slide id={mode} index={mode === "compose" ? 1 : 0} inner="grid gap-6 max-md:gap-4">
      {mode === "compose" ? (
        <>
          {top?.("compose")}
          <Field label={t.containerForm.compose} htmlFor="compose" help={t.containerForm.composeHelp}>
            <Textarea id="compose" value={compose} onChange={(e) => setCompose(e.target.value)} spellCheck={false} className={composeInputClass} required />
          </Field>
          <MoreSettings label={t.containerForm.moreCompose(!first)} labelOpen={t.containerForm.more}>
            <FormRow className="gap-y-6 max-md:gap-y-4">
              {moreTop}
              {deps}
            </FormRow>
          </MoreSettings>
        </>
      ) : (
        imageForm.fields
      )}
      </Slide>
      )}
    </>
  );
  return { fields, build, mode, image: () => (mode === "image" ? imageForm.build().image ?? "" : "") };
}
