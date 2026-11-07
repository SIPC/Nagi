// 导入现有目录（见 docs/agent.md#现有目录）：填节点上的目录、名称、节点，预览后接入为容器组，数据留在原处。
// 只给管理员；读的是主机上的文件，要先确认身份。
import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Alert, Button, ButtonLink, Field, FormActions, FormRow, HelpTip, Input, KeyValue, LogBox, Notice, Spinner } from "@/components/ui";
import { isElevationRequired, requireElevation } from "@/components/elevate";
import { errorMessage, nodeClient, projectClient } from "@/lib/api";
import type { ComposeDirPreview } from "@/gen/nagi/v1/api_pb";
import type { Project } from "@/gen/nagi/v1/common_pb";
import { useNodeChoice } from "@/pages/projects";
import { t } from "@/i18n";

// elevated 先调用一次，要确认身份时确认后再调一次。
async function elevated<T>(call: () => Promise<T>): Promise<T> {
  try {
    return await call();
  } catch (err) {
    if (!isElevationRequired(err) || !(await requireElevation(t.importDir.elevate))) throw err;
    return call();
  }
}

export function ImportDir({ project }: { project: Project }) {
  const k = t.importDir;
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [dir, setDir] = useState("");
  const [name, setName] = useState("");
  const [backupPaths, setBackupPaths] = useState("");
  const [preview, setPreview] = useState<ComposeDirPreview>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<"" | "read" | "deploy" | "add">("");
  const node = useNodeChoice({ projectId: project.id, projectName: project.networkName, container: name || "app", preferNodes: project.containers.map((m) => m.nodeId) });
  // 预览读的是选中节点上的目录，换节点后作废，要重新读取。
  useEffect(() => setPreview(undefined), [node.nodeId]);

  const read = async () => {
    setBusy("read");
    setError("");
    setPreview(undefined);
    try {
      const p = await elevated(() => nodeClient.inspectComposeDir({ nodeId: node.nodeId, path: dir.trim() }));
      setPreview(p);
      setBackupPaths(p.backupPaths.join(", "));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy("");
    }
  };
  const add = async (deploy: boolean) => {
    if (!preview) return;
    const finalName = name.trim() || preview.name;
    setBusy(deploy ? "deploy" : "add");
    setError("");
    try {
      await elevated(() =>
        projectClient.importComposeDir({ projectId: project.id, nodeId: node.nodeId, dir: preview.path, name: finalName, backupPaths: backupPaths.split(/[\s,]+/).filter(Boolean) }),
      );
      await queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      navigate({ to: "/projects/$projectId/c/$container", params: { projectId: project.id, container: finalName }, search: deploy ? { deploy: true } : {}, ignoreBlocker: true });
    } catch (err) {
      setError(errorMessage(err));
      setBusy("");
    }
  };

  return (
    <form
      className="grid gap-6 max-md:gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (preview) add(true);
        else read();
      }}
    >
      <p className="text-muted-foreground">{k.description}</p>
      <FormRow>
        <Field label={k.dir} htmlFor="import-dir" help={k.dirHelp}>
          <Input
            id="import-dir"
            value={dir}
            onChange={(e) => {
              setDir(e.target.value);
              setPreview(undefined);
            }}
            placeholder="/opt/immich"
            required
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
        {node.nodeField}
      </FormRow>
      <FormRow>
        <Field label={t.projects.create.groupName} htmlFor="import-name" help={t.containerForm.groupNameHelp}>
          <Input id="import-name" value={name} onChange={(e) => setName(e.target.value)} placeholder={preview?.name || "app"} autoComplete="off" spellCheck={false} />
        </Field>
        {preview && (
          <Field label={t.projectContainers.config.backupPaths} htmlFor="import-backup" help={t.projectContainers.config.backupPathsHelp(preview.path)}>
            <Input id="import-backup" value={backupPaths} onChange={(e) => setBackupPaths(e.target.value)} placeholder="postgres" autoComplete="off" spellCheck={false} />
          </Field>
        )}
      </FormRow>
      {preview && (
        <div className="grid gap-4">
          <KeyValue
            items={[
              [k.composeFile, preview.composeFile],
              [
                <span className="flex items-center gap-1">
                  {k.variables}
                  <HelpTip>{k.variablesHelp}</HelpTip>
                </span>,
                preview.variables.length ? t.common.list(preview.variables) : k.none,
              ],
              [k.replaces, preview.containers.length ? k.replacesValue(preview.project, t.common.list(preview.containers)) : k.nothingRunning],
            ]}
          />
          {preview.containers.length > 0 && <Notice tone="warning">{k.replaceNotice}</Notice>}
          {preview.notes.map((n) => (
            <Notice key={n}>{n}</Notice>
          ))}
          <Notice>{backupPaths.trim() ? k.limitsWithBackup : k.limits}</Notice>
          <LogBox className="max-h-72">{preview.compose}</LogBox>
        </div>
      )}
      {error && <Alert>{error}</Alert>}
      <FormActions className="mt-2 max-md:grid max-md:[&>*]:w-full">
        {preview ? (
          <>
            <Button type="submit" disabled={!!busy}>
              {busy === "deploy" && <Spinner />}
              {k.importAndDeploy}
            </Button>
            <Button variant="outline" disabled={!!busy} onClick={() => add(false)}>
              {busy === "add" && <Spinner />}
              {t.projectContainers.add.addOnly}
            </Button>
          </>
        ) : (
          <Button type="submit" disabled={!!busy || !node.nodeId}>
            {busy === "read" && <Spinner />}
            {k.read}
          </Button>
        )}
        <ButtonLink to="/projects/$projectId" params={{ projectId: project.id }} variant="ghost" readOnlyOk>
          {t.common.actions.cancel}
        </ButtonLink>
      </FormActions>
    </form>
  );
}
