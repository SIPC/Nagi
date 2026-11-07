import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { ChevronRight, Plus } from "lucide-react";
import { toast } from "sonner";
import { errorMessage, knownNodes, nodeClient, projectClient, toDate } from "@/lib/api";
import { capitalize, formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";
import { useRun } from "@/components/run";
import { fmtBytes } from "@/components/metrics";
import {
  Alert,
  Button,
  ButtonLink,
  Check,
  Dialog,
  DialogActions,
  EmptyRow,
  Field,
  FormRow,
  Input,
  KeyValue,
  List,
  Notice,
  QueryError,
  Roll,
  Row,
  RowMenu,
  RowsSkeleton,
  Section,
  Select,
  SettingRow,
  Spinner,
  Status,
} from "@/components/ui";
import type { Container, Project } from "@/gen/nagi/v1/common_pb";
import type { VolumeInfo } from "@/gen/nagi/v1/agent_pb";
import { useDefaultDir } from "@/pages/projects";
import { confirmAction } from "@/components/confirm";
import { AlertDialog, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { useRole } from "@/components/role";
import { snapshotTime, VolumeFiles } from "@/pages/volume-files";
import { defaultSnapshotIntervalMinutes, defaultSnapshotLimit } from "@/lib/config";

// 容器的数据：快照、顶替、备份节点、更换节点（迁移、从备份节点恢复、重新部署到其他节点）。
// 见 docs/snapshot.md、docs/failover.md。

const snapshotKinds: Record<string, string> = {
  scheduled: t.projectData.snapshotKinds.scheduled,
  manual: t.projectData.snapshotKinds.manual,
  predeploy: t.projectData.snapshotKinds.predeploy,
  version: t.projectData.snapshotKinds.version,
};

function useNodes() {
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => nodeClient.listNodes({}),
    refetchInterval: 10000,
  });
  const list = nodes.data?.nodes ?? [];
  const known = knownNodes(nodes.data);
  return {
    list,
    // pending、loadError：还不知道节点在不在线，这时不能下「节点离线」的结论。
    pending: nodes.isPending,
    loadError: nodes.data ? null : nodes.error,
    refetch: nodes.refetch,
    // fallback：顶替设置里记的名字。节点改过名时设置里的是旧名字，所以先按 ID 查当前的名字。
    name: (id: string, fallback?: string) => known.find((n) => n.id === id)?.name ?? fallback ?? id,
    online: (id: string) => known.find((n) => n.id === id)?.online === true,
    usable: list.filter((n) => n.online && !n.pending),
  };
}

type Nodes = ReturnType<typeof useNodes>;

// useCall：表单和按钮共用的提交：转圈、错误、成功后刷新项目。返回是否成功。
// useCall：区块里的操作。pending 记的是哪个操作在进行（call 的第二个参数），转圈只出现在被点的按钮上；busy 是有操作在进行。
function useCall(projectId: string) {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState("");
  const [error, setError] = useState("");
  const call = async (fn: () => Promise<unknown>, key = "call") => {
    setPending(key);
    setError("");
    try {
      await fn();
      queryClient.invalidateQueries({ queryKey: ["project", projectId] });
      return true;
    } catch (err) {
      setError(errorMessage(err));
      return false;
    } finally {
      setPending("");
    }
  };
  return { busy: pending !== "", pending, error, call };
}

// ContainerStorage：容器页的「存储」标签：卷和快照；地址带 volume 时就地换成这个卷的文件管理。
export function ContainerStorage({
  project,
  container,
}: {
  project: Project;
  container: Container;
}) {
  const run = useRun(project.id);
  const { isViewer } = useRole();
  // 这个组件不绑定某条路由，volume 由容器页的路由校验。
  const { volume } = useSearch({ strict: false }) as { volume?: string };
  const navigate = useNavigate();
  const openVolume = (name?: string) => navigate({ to: ".", search: ((prev: Record<string, unknown>) => ({ ...prev, volume: name })) as never });
  // 只读角色没有文件管理的入口，直接打开链接也回到列表。
  const browsing = volume && !isViewer ? volume : undefined;
  return (
    <div className="grid grid-cols-1 gap-8">
      {run.output && <div className="empty:hidden">{run.output}</div>}
      {browsing ? (
        <BrowseVolume key={`${container.name}/${browsing}`} project={project} container={container} volume={browsing} onClose={() => openVolume()} />
      ) : (
        <div className="min-w-0">
          <Volumes project={project} container={container} onBrowse={openVolume} />
          <Snapshots project={project} container={container} run={run} />
        </div>
      )}
    </div>
  );
}

// ContainerHA：容器页的「高可用」标签：先看状态，设置放进对话框。需要处理的事在最上面。
export function ContainerHA({
  project,
  container,
}: {
  project: Project;
  container: Container;
}) {
  const run = useRun(project.id);
  const nodes = useNodes();
  if (nodes.pending) return <RowsSkeleton />;
  if (nodes.loadError) return <QueryError error={errorMessage(nodes.loadError)} onRetry={() => nodes.refetch()} />;
  const restorable = !!container.backupNodeId && !nodes.online(container.nodeId) && container.deployedVersion > 0n;
  return (
    <div className="grid grid-cols-1 gap-8">
      {run.output && <div className="empty:hidden">{run.output}</div>}
      {container.failoverStandby?.pendingConfirm && <TakeoverNotice project={project} container={container} nodes={nodes} />}
      {restorable && <RestoreNotice project={project} container={container} nodes={nodes} run={run} />}
      <div className="min-w-0">
        {container.failover && <FailoverDetail project={project} container={container} nodes={nodes} />}
        {container.backupNodeId && <BackupDetail project={project} container={container} nodes={nodes} />}
        {restorable && <RestorableSnapshots project={project} container={container} nodes={nodes} run={run} />}
        <Section>
          <List>
            {!container.failover && <FailoverRow project={project} container={container} nodes={nodes} />}
            {!container.backupNodeId && <BackupRow project={project} container={container} nodes={nodes} />}
            <PlacementRow project={project} container={container} nodes={nodes} run={run} />
          </List>
        </Section>
      </div>
    </div>
  );
}

type Run = ReturnType<typeof useRun>;

// ---- 卷 ----

// volumeLabel：卷的标题。应用文件叫「应用文件」；单个容器的卷名是按容器内路径自动起的，直接显示容器内路径。
const volumeLabel = (v: VolumeInfo, kind?: string) =>
  v.kind === "data" ? t.projectData.volumes.appFiles : kind !== "group" && v.kind === "managed" && v.mounts.length === 1 ? v.mounts[0].target : v.name;
// 没有文件、也没有挂进容器的应用文件目录不显示（只用镜像、不带文件的容器用不到它）。
const shownVolume = (v: VolumeInfo) => v.kind !== "data" || v.mounts.length > 0 || v.files > 0n;

// useVolumes：卷的统计和持久化目录。数据在节点上，所以每次打开都去节点统计一次。
function useVolumes(project: Project, container: Container, nodes: Nodes) {
  const queryClient = useQueryClient();
  const m = t.projectData.volumes;
  const volumes = useQuery({
    queryKey: ["volumes", project.id, container.name],
    queryFn: () => projectClient.listVolumes({ projectId: project.id, container: container.name }),
    enabled: nodes.online(container.nodeId) && container.deployedVersion > 0n,
    refetchInterval: 30000,
  });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  // 单个容器的持久化目录可以在这里改，走草稿；容器组由 compose 定义。
  const cfg = container.draft ?? container.config;
  const form = cfg?.form;
  const targets = form?.volumes ?? [];
  const saveTargets = async (next: string[]) => {
    if (!cfg || !form) return false;
    setBusy(true);
    setError("");
    try {
      const res = await projectClient.saveDraft({
        projectId: project.id,
        container: container.name,
        baseRevision: container.revision,
        config: { ...cfg, form: { ...form, volumes: next } },
      });
      toast.success(res.containers.find((c) => c.name === container.name)?.hasDraft ? m.savedDraft : m.noChanges);
      queryClient.invalidateQueries({ queryKey: ["project", project.id] });
      return true;
    } catch (err) {
      setError(errorMessage(err));
      return false;
    } finally {
      setBusy(false);
    }
  };
  const remove = async (target: string) => {
    if (await confirmAction(m.removeConfirm(target), { action: m.remove, danger: true })) await saveTargets(targets.filter((x) => x !== target));
  };
  // removableMount：这个卷里还在配置中的挂载，取消挂载要的就是它。
  const removableMount = (v: VolumeInfo) => (form ? v.mounts.find((x) => targets.includes(x.target)) : undefined);
  return { volumes, form, targets, saveTargets, remove, removableMount, busy, error };
}

// BrowseVolume：文件管理页。卷名和能否取消挂载从卷列表来，所以直接打开链接也能用。
function BrowseVolume({ project, container, volume, onClose }: { project: Project; container: Container; volume: string; onClose: () => void }) {
  const nodes = useNodes();
  const v = useVolumes(project, container, nodes);
  const info = v.volumes.data?.volumes.find((x) => x.name === volume);
  const mount = info && v.removableMount(info);
  return (
    <div className="grid min-w-0 gap-4">
      {!nodes.pending && !nodes.loadError && !nodes.online(container.nodeId) && <Notice tone="warning">{t.projectData.volumes.nodeOffline(nodes.name(container.nodeId))}</Notice>}
      {v.error && <Alert>{v.error}</Alert>}
      <VolumeFiles
        project={project}
        container={container}
        volume={volume}
        label={info ? volumeLabel(info, container.kind) : volume}
        onClose={onClose}
        unmount={mount && { disabled: v.busy, onSelect: () => v.remove(mount.target) }}
      />
    </div>
  );
}

function Volumes({ project, container, onBrowse }: { project: Project; container: Container; onBrowse: (volume: string) => void }) {
  const nodes = useNodes();
  const { isViewer } = useRole();
  const m = t.projectData.volumes;
  const online = nodes.online(container.nodeId);
  const deployed = container.deployedVersion > 0n;
  const { volumes, form, targets, saveTargets, removableMount, busy, error } = useVolumes(project, container, nodes);
  const [adding, setAdding] = useState(false);
  const [path, setPath] = useState("");
  // 提交过一次以后才显示路径格式的提示。
  const [checked, setChecked] = useState(false);
  const pathError = checked && !path.trim().startsWith("/") ? m.addInvalid : "";

  const deployedTargets = new Set((volumes.data?.volumes ?? []).flatMap((v) => v.mounts.map((x) => x.target)));
  const pendingAdd = targets.filter((x) => !deployedTargets.has(x));
  const disk = volumes.data;
  const closeAdd = () => {
    setAdding(false);
    setPath("");
    setChecked(false);
  };

  const sub = (v: VolumeInfo) => {
    const mounts = t.common.list(v.mounts.map((x) => (container.kind === "group" ? `${x.service}: ${x.target}` : x.target)));
    const files = m.files(Number(v.files));
    const removing = form && v.kind === "managed" && v.mounts.length > 0 && !removableMount(v);
    const titled = volumeLabel(v, container.kind) !== v.name;
    const parts =
      v.kind === "data"
        ? ["data/", mounts, files]
        : v.kind === "managed" && titled
          ? [removing ? m.pendingRemove : "", files]
        : v.kind === "unmanaged"
          ? [mounts, m.kinds.unmanagedSub]
          : v.kind === "orphan"
            ? [mounts, m.kinds.orphanSub, files]
            : [mounts, removing ? m.pendingRemove : "", files];
    return parts.filter(Boolean).join(" · ");
  };

  // volumeActions：浏览。取消挂载在卷的文件页右上角的更多里。
  const volumeActions = (v: VolumeInfo) =>
    v.kind !== "unmanaged" && (
      <Button variant="ghost" size="row" onClick={() => onBrowse(v.name)}>
        {m.browse}
      </Button>
    );

  return (
    <Section
      title={m.title}
      help={m.help}
      actions={
        <>
          {container.kind === "group" && (
            <span>
              {m.composeHint} ·{" "}
              <Link to="/projects/$projectId/c/$container" params={{ projectId: project.id, container: container.name }} search={{ view: "config" }} className="hover:underline">
                {m.composeLink}
              </Link>
            </span>
          )}
          {disk && disk.diskTotalBytes > 0n && <Roll value={m.diskFree(fmtBytes(Number(disk.diskFreeBytes)), fmtBytes(Number(disk.diskTotalBytes)))} n={Number(disk.diskFreeBytes)} />}
          {form && !isViewer && (
            <Button variant="outline" onClick={() => setAdding(true)}>
              <Plus />
              {m.addDir}
            </Button>
          )}
        </>
      }
    >
      {adding && (
        <form
          className="grid gap-2"
          onSubmit={async (e) => {
            e.preventDefault();
            setChecked(true);
            if (!path.trim().startsWith("/")) return;
            if (await saveTargets([...targets, path.trim()])) closeAdd();
          }}
        >
          <div className="flex items-center gap-2">
            <Input
              autoFocus
              value={path}
              onChange={(e) => setPath(e.target.value)}
              placeholder={m.addPlaceholder}
              aria-label={m.addLabel}
              aria-invalid={!!pathError}
              aria-describedby="add-dir-hint"
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              className="min-w-0 flex-1"
            />
            <Button type="submit" disabled={busy}>
              {busy && <Spinner />}
              {t.common.actions.add}
            </Button>
            <Button variant="ghost" onClick={closeAdd}>
              {t.common.actions.cancel}
            </Button>
          </div>
          {pathError ? (
            <p id="add-dir-hint" role="alert" className="leading-5 text-destructive">
              {pathError}
            </p>
          ) : (
            <p id="add-dir-hint" className="text-[13px] leading-5 text-muted-foreground">
              {m.addHint}
            </p>
          )}
        </form>
      )}
      {error && <Alert>{error}</Alert>}
      {!deployed ? (
        <NotDeployed project={project} container={container} />
      ) : nodes.pending ? (
        <RowsSkeleton />
      ) : nodes.loadError ? (
        <QueryError error={errorMessage(nodes.loadError)} onRetry={() => nodes.refetch()} />
      ) : !online ? (
        <Notice tone="warning">{m.nodeOffline(nodes.name(container.nodeId))}</Notice>
      ) : volumes.isPending ? (
        <RowsSkeleton />
      ) : volumes.error ? (
        <QueryError error={errorMessage(volumes.error)} onRetry={() => volumes.refetch()} />
      ) : volumes.data.volumes.filter(shownVolume).length === 0 && pendingAdd.length === 0 ? (
        <EmptyRow>{m.empty}</EmptyRow>
      ) : (
        <List>
          {volumes.data.volumes.filter(shownVolume).map((v) => (
            <Row
              key={v.name}
              title={
                <span className="truncate" title={v.hostPath}>
                  {volumeLabel(v, container.kind)}
                </span>
              }
              sub={
                <span title={v.kind === "unmanaged" ? m.kinds.unmanagedHint : v.kind === "orphan" ? m.kinds.orphanHint : undefined}>{sub(v)}</span>
              }
              cols={[
                { content: v.kind === "unmanaged" ? "" : <Roll value={`${v.partial ? ">" : ""}${fmtBytes(Number(v.sizeBytes))}`} n={Number(v.sizeBytes)} />, num: true },
                { content: v.kind !== "unmanaged" && v.modified ? <span title={formatDateTime(toDate(v.modified))}>{capitalize(timeAgo(toDate(v.modified)))}</span> : "" },
              ]}
              actionsWidth="w-28"
              actions={isViewer ? null : volumeActions(v)}
            />
          ))}
          {pendingAdd.map((target) => (
            <Row
              key={target}
              title={<span className="truncate">{target}</span>}
              sub={<span className="text-warning-text">{m.pendingAdd}</span>}
              cols={[{ content: "", num: true }, { content: "" }]}
              actionsWidth="w-28"
              actions={
                !isViewer ? (
                  <Button variant="ghost" size="row" disabled={busy} onClick={() => saveTargets(targets.filter((x) => x !== target))}>
                    {t.common.actions.remove}
                  </Button>
                ) : null
              }
            />
          ))}
        </List>
      )}
    </Section>
  );
}

// NotDeployed：未部署时卷和快照的空状态，给出部署按钮（进入概览开始部署）。
function NotDeployed({ project, container }: { project: Project; container: Container }) {
  return (
    <EmptyRow
      action={
        <ButtonLink to="/projects/$projectId/c/$container" params={{ projectId: project.id, container: container.name }} search={{ view: "overview", deploy: true }} variant="outline">
          {t.projectData.deploy}
        </ButtonLink>
      }
    >
      {t.projectData.notDeployed}
    </EmptyRow>
  );
}

// ---- 快照 ----

// interval：快照间隔，整小时的写成小时。
const interval = (min: number) => (min % 60 === 0 ? t.common.time.hours(min / 60) : t.common.time.minutes(min));

function Snapshots({
  project,
  container,
  run,
}: {
  project: Project;
  container: Container;
  run: Run;
}) {
  const nodes = useNodes();
  const s = t.projectData.snapshots;
  const deployed = container.deployedVersion > 0n;
  const online = nodes.online(container.nodeId);
  return (
    <Section
      title={s.title}
      help={s.help}
      actions={
        <>
          <span>
            {s.scheduleShort(interval(project.settings?.snapshotIntervalMinutes || defaultSnapshotIntervalMinutes))}
            {s.limitShort(project.settings?.snapshotLimit || defaultSnapshotLimit)} ·{" "}
            <Link to="/projects/$projectId" params={{ projectId: project.id }} search={{ tab: "settings" }} className="hover:underline">
              {s.scheduleLink}
            </Link>
          </span>
          {deployed && online && (
            <Button
              variant="outline"
              disabled={run.busy}
              onClick={() =>
                run.run(() =>
                  projectClient.createSnapshot({
                    projectId: project.id,
                    container: container.name,
                  }),
                )
              }
            >
              {run.busy && <Spinner />}
              {s.create}
            </Button>
          )}
        </>
      }
    >
      {!deployed ? (
        <NotDeployed project={project} container={container} />
      ) : nodes.pending ? (
        <RowsSkeleton />
      ) : nodes.loadError ? (
        <QueryError error={errorMessage(nodes.loadError)} onRetry={() => nodes.refetch()} />
      ) : !online ? (
        <Notice tone="warning">{s.nodeOffline(nodes.name(container.nodeId))}</Notice>
      ) : (
        <SnapshotList project={project} container={container} busy={run.busy} run={run.run} />
      )}
    </Section>
  );
}

// SnapshotRows：一行是绝对时间加相对时间、类型、大小、版本和操作。卷和快照共用一套列位：
// 大小都在倒数第二列，操作列同宽（w-28），大小才对得齐。
// action 返回 null 的行（不能在这里操作的快照）显示「随版本回滚」；返回空字符串时不显示操作。
function SnapshotRows({
  snapshots,
  action,
}: {
  snapshots: {
    id: string;
    kind: string;
    time?: Parameters<typeof toDate>[0];
    version: bigint;
    size: bigint;
  }[];
  action: (id: string, kind: string) => React.ReactNode;
}) {
  const s = t.projectData.snapshots;
  // 默认只列最近几份，其余收起。
  const [all, setAll] = useState(false);
  if (snapshots.length === 0) return <EmptyRow>{s.empty}</EmptyRow>;
  const shown = all ? snapshots : snapshots.slice(0, 6);
  return (
    <>
      <List>
        {shown.map((sn) => (
          <Row
            key={sn.id}
            title={
              <>
                <span className="shrink-0" title={formatDateTime(toDate(sn.time))}>
                  {snapshotTime(toDate(sn.time))}
                </span>
                <span className="truncate text-muted-foreground">{capitalize(timeAgo(toDate(sn.time)))}</span>
              </>
            }
            cols={[
              { content: snapshotKinds[sn.kind] ?? sn.kind },
              { content: fmtBytes(Number(sn.size)), num: true },
              { content: sn.version > 0n ? `v${sn.version}` : "" },
            ]}
            actionsWidth="w-28"
            actions={
              action(sn.id, sn.kind) ?? (
                <span className="text-muted-foreground" title={s.viaVersionHint}>
                  {s.viaVersion}
                </span>
              )
            }
          />
        ))}
      </List>
      {snapshots.length > shown.length && (
        <div>
          <Button readOnlyOk variant="ghost" className="-ml-3" onClick={() => setAll(true)}>
            {s.showAll(snapshots.length)}
          </Button>
        </div>
      )}
    </>
  );
}

function SnapshotList({
  project,
  container: c,
  busy,
  run,
}: {
  project: Project;
  container: Container;
  busy: boolean;
  run: Run["run"];
}) {
  const container = c.name;
  const s = t.projectData.snapshots;
  const queryClient = useQueryClient();
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState("");
  // 容器换了节点（目录）以后是另一个快照仓库。
  const queryKey = ["snapshots", project.id, container, c.nodeId, c.dir];
  const snapshots = useQuery({
    queryKey,
    queryFn: () =>
      projectClient.listSnapshots({ projectId: project.id, container }),
  });
  if (snapshots.isPending) return <RowsSkeleton />;
  if (snapshots.error) return <QueryError error={errorMessage(snapshots.error)} onRetry={() => snapshots.refetch()} />;
  const restore = async (id: string) =>
    (await confirmAction(s.restoreConfirm(container, t.common.list(c.config?.backupPaths ?? [])), { action: s.restore, danger: true })) &&
    run(() => projectClient.restoreSnapshot({ projectId: project.id, container, snapshotId: id }));
  const remove = async (id: string) => {
    if (!(await confirmAction(s.deleteConfirm, { action: t.common.actions.delete, danger: true }))) return;
    setDeleting(true);
    setError("");
    try {
      const res = await projectClient.deleteSnapshot({ projectId: project.id, container, snapshotId: id });
      queryClient.setQueryData(queryKey, res);
      // 文件管理的来源选择单独缓存了一份列表。
      queryClient.invalidateQueries({ queryKey: ["snapshots", project.id, container, "files"] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setDeleting(false);
    }
  };
  // 版本文件不能删，也不在这里恢复；部署前数据只能删，恢复随版本回滚。
  return (
    <>
      {error && <Alert>{error}</Alert>}
      <SnapshotRows
        snapshots={snapshots.data.snapshots}
        action={(id, kind) =>
          kind === "version" ? null : (
            <RowMenu
              items={[
                ...(kind === "scheduled" || kind === "manual" ? [{ label: s.restore, disabled: busy || deleting, onSelect: () => restore(id) }] : []),
                { label: t.common.actions.delete, danger: true, disabled: busy || deleting, onSelect: () => remove(id) },
              ]}
            />
          )
        }
      />
    </>
  );
}

// ---- 备份节点 ----

// BackupRow：还没有备份节点时的一行。
function BackupRow({ project, container, nodes }: { project: Project; container: Container; nodes: Nodes }) {
  const b = t.projectData.backup;
  const [open, setOpen] = useState(false);
  const candidates = nodes.usable.filter((n) => n.id !== container.nodeId);
  const available = !container.failover && candidates.length > 0;
  return (
    <>
      <SettingRow label={b.title} help={b.help} description={container.failover ? b.hasFailover : candidates.length === 0 ? b.noCandidates : b.unset}>
        {available ? (
          <Button variant="outline" onClick={() => setOpen(true)}>
            {t.projectData.setUp}
          </Button>
        ) : null}
      </SettingRow>
      {open && <BackupDialog project={project} container={container} nodes={nodes} onClose={() => setOpen(false)} />}
    </>
  );
}

function BackupDialog({ project, container, nodes, onClose }: { project: Project; container: Container; nodes: Nodes; onClose: () => void }) {
  const b = t.projectData.backup;
  const { busy, error, call } = useCall(project.id);
  const candidates = nodes.usable.filter((n) => n.id !== container.nodeId);
  const [nodeId, setNodeId] = useState("");
  const choice = nodeId || candidates[0]?.id || "";
  return (
    <Dialog open onClose={onClose} title={b.set} description={b.description}>
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          if (await call(() => projectClient.setBackupNode({ projectId: project.id, container: container.name, nodeId: choice, deleteReplica: false }))) onClose();
        }}
      >
        <Field label={b.node} htmlFor="backup-node">
          <Select id="backup-node" value={choice} onChange={(e) => setNodeId(e.target.value)}>
            {candidates.map((n) => (
              <option key={n.id} value={n.id}>
                {n.name}
              </option>
            ))}
          </Select>
        </Field>
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          <Button variant="outline" readOnlyOk onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy || !choice}>
            {busy && <Spinner />}
            {b.set}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// BackupDetail：已设置备份节点：标签值列表，同步和取消在标题栏。
function BackupDetail({ project, container, nodes }: { project: Project; container: Container; nodes: Nodes }) {
  const b = t.projectData.backup;
  const { busy, pending, error, call } = useCall(project.id);
  const backup = container.backup;
  const sourceOnline = nodes.online(container.nodeId);

  let state: React.ReactNode = <Status tone="neutral">{b.waitingFirstSync}</Status>;
  if (!sourceOnline) state = <Status tone="neutral">{b.sourceOffline(nodes.name(container.nodeId))}</Status>;
  else if (backup?.syncing)
    state = (
      <Status tone="info" pulse>
        {t.common.status.syncing}
      </Status>
    );
  else if (backup?.lastError) state = <Status tone="danger">{b.syncFailed}</Status>;
  else if (backup?.lastSync) state = <Status tone="success">{b.healthy}</Status>;

  return (
    <Section
      title={b.title}
      help={b.help}
      actions={
        <>
          <Button
            variant="outline"
            disabled={busy || !sourceOnline || backup?.syncing}
            title={sourceOnline ? undefined : b.sourceOffline(nodes.name(container.nodeId))}
            onClick={() => call(() => projectClient.syncBackup({ projectId: project.id, container: container.name }), "sync")}
          >
            {pending === "sync" && <Spinner />}
            {b.syncNow}
          </Button>
          <Button
            variant="outline"
            disabled={busy}
            onClick={async () =>
              (await confirmAction(b.removeConfirm(container.name), { action: b.remove, danger: true })) &&
              call(() => projectClient.setBackupNode({ projectId: project.id, container: container.name, nodeId: "", deleteReplica: true }), "remove")
            }
          >
            {pending === "remove" && <Spinner />}
            {b.remove}
          </Button>
        </>
      }
    >
      <KeyValue
        items={[
          [
            b.node,
            <span>
              {nodes.name(container.backupNodeId)}
              {!nodes.online(container.backupNodeId) && <span className="text-muted-foreground"> {t.common.status.offline}</span>}
            </span>,
          ],
          // 所在节点离线时最上面已有提示，这里不再重复状态。
          ...(sourceOnline || container.deployedVersion === 0n ? ([[t.projectData.state, state]] as [React.ReactNode, React.ReactNode][]) : []),
          [t.projectData.lastSync, <span title={formatDateTime(toDate(backup?.lastSync))}>{backup?.lastSync ? capitalize(timeAgo(toDate(backup.lastSync))) : "—"}</span>],
        ]}
      />
      {backup?.lastError && (
        <Notice tone="danger" title={b.syncFailed}>
          {backup.lastError}
        </Notice>
      )}
      {error && <Alert>{error}</Alert>}
    </Section>
  );
}


// RestoreNotice：所在节点离线、有备份节点时最上面的提示。
function RestoreNotice({ project, container, nodes, run }: { project: Project; container: Container; nodes: Nodes; run: Run }) {
  const [open, setOpen] = useState(false);
  const backupName = nodes.name(container.backupNodeId);
  return (
    <>
      <Notice
        tone="warning"
        title={t.projectData.placement.offline(nodes.name(container.nodeId))}
        actions={
          <Button disabled={run.busy} onClick={() => setOpen(true)}>
            {t.projectData.restore.latest}
          </Button>
        }
      >
        {t.projectData.restore.hint(backupName)}
      </Notice>
      {open && <RestoreDialog project={project} container={container} nodes={nodes} run={run} snapshotId="" onClose={() => setOpen(false)} />}
    </>
  );
}

function RestorableSnapshots({ project, container, nodes, run }: { project: Project; container: Container; nodes: Nodes; run: Run }) {
  const [restoring, setRestoring] = useState<string | null>(null);
  const snapshots = useQuery({
    queryKey: ["backup-snapshots", project.id, container.name],
    queryFn: () =>
      projectClient.listBackupSnapshots({
        projectId: project.id,
        container: container.name,
      }),
  });
  return (
    <Section title={t.projectData.restore.snapshots}>
      {snapshots.isPending ? (
        <RowsSkeleton />
      ) : snapshots.error ? (
        <QueryError error={errorMessage(snapshots.error)} onRetry={() => snapshots.refetch()} />
      ) : (
        <SnapshotRows
          snapshots={snapshots.data.snapshots}
          action={(id, kind) =>
            kind === "scheduled" || kind === "manual" ? (
              <Button variant="ghost" size="row" disabled={run.busy} onClick={() => setRestoring(id)}>
                {t.projectData.snapshots.restore}
              </Button>
            ) : (
              // 原节点离线，没法回滚版本，这里不显示「随版本回滚」。
              ""
            )
          }
        />
      )}
      {restoring !== null && <RestoreDialog project={project} container={container} nodes={nodes} run={run} snapshotId={restoring} onClose={() => setRestoring(null)} />}
    </Section>
  );
}

// 确认文案的第一句（到第一个问号）作标题，其余作说明，和确认框的写法一致。
function splitConfirm(message: string): [string, string] {
  const i = message.search(/[？?]/); // i18n-ignore
  return i < 0 ? [message, ""] : [message.slice(0, i + 1), message.slice(i + 1).trim()];
}

// RestoreDialog：在备份节点上恢复的确认框，多一个恢复目录，所以不能用 confirmAction。snapshotId 为空表示最新快照。
function RestoreDialog({ project, container, nodes, run, snapshotId, onClose }: { project: Project; container: Container; nodes: Nodes; run: Run; snapshotId: string; onClose: () => void }) {
  const r = t.projectData.restore;
  const [dir, setDir] = useState("");
  const def = useDefaultDir(container.backupNodeId, project.id, project.networkName, container.name);
  const [title, body] = splitConfirm(r.confirm(nodes.name(container.backupNodeId), container.name));
  return (
    <AlertDialog open onOpenChange={(v) => !v && onClose()}>
      <AlertDialogContent>
        <div className="grid gap-1">
          <AlertDialogTitle className="text-base">{title}</AlertDialogTitle>
          <AlertDialogDescription>{body}</AlertDialogDescription>
        </div>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            onClose();
            run.run(() =>
              projectClient.restoreFromBackup({
                projectId: project.id,
                container: container.name,
                snapshotId,
                dir: dir.trim() || def,
              }),
            );
          }}
        >
          <Field label={r.dir} htmlFor="restore-dir" help={t.projectData.dirHelp}>
            <Input id="restore-dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder={def} autoComplete="off" spellCheck={false} />
          </Field>
          <DialogActions>
            <AlertDialogCancel asChild>
              <Button variant="outline" readOnlyOk>
                {t.common.actions.cancel}
              </Button>
            </AlertDialogCancel>
            <Button type="submit" variant="destructive" disabled={run.busy}>
              {snapshotId ? r.fromSnapshot : r.latest}
            </Button>
          </DialogActions>
        </form>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// ---- 更换节点 ----

// PlacementRow：所在节点一行。在线时迁移；离线时只能重新部署（数据不随容器转移），红字按钮。
// 离线且有备份节点时先给「从备份节点恢复」，重新部署收进菜单。
function PlacementRow({ project, container, nodes, run }: { project: Project; container: Container; nodes: Nodes; run: Run }) {
  const p = t.projectData.placement;
  const [open, setOpen] = useState(false);
  const [restoring, setRestoring] = useState(false);
  const candidates = nodes.usable.filter((n) => n.id !== container.nodeId);
  const online = nodes.online(container.nodeId);
  const name = nodes.name(container.nodeId);
  const restorable = !online && !!container.backupNodeId && container.deployedVersion > 0n;
  const note = container.failover ? p.hasFailover : restorable ? p.backupHint : candidates.length === 0 ? p.noCandidates : online ? "" : p.redeployHint;
  const available = !container.failover && candidates.length > 0;
  return (
    <>
      <SettingRow
        label={p.currentNode}
        help={p.help}
        description={[online ? name : p.offline(name), container.dir, note].filter(Boolean).join(" · ")}
      >
        {restorable ? (
          <div className="flex items-center gap-1">
            <Button variant="outline" disabled={run.busy} onClick={() => setRestoring(true)}>
              {t.projectData.backup.restoreFromBackup}
            </Button>
            {available && <RowMenu items={[{ label: p.redeployTitle, danger: true, disabled: run.busy, onSelect: () => setOpen(true) }]} />}
          </div>
        ) : available ? (
          <Button variant={online ? "outline" : "destructive-outline"} disabled={run.busy} onClick={() => setOpen(true)}>
            {online ? p.migrate : p.redeployShort}
          </Button>
        ) : null}
      </SettingRow>
      {open && <PlacementDialog project={project} container={container} nodes={nodes} run={run} online={online} onClose={() => setOpen(false)} />}
      {restoring && <RestoreDialog project={project} container={container} nodes={nodes} run={run} snapshotId="" onClose={() => setRestoring(false)} />}
    </>
  );
}

function PlacementDialog({ project, container, nodes, run, online, onClose }: { project: Project; container: Container; nodes: Nodes; run: Run; online: boolean; onClose: () => void }) {
  const p = t.projectData.placement;
  const [nodeId, setNodeId] = useState("");
  const [dir, setDir] = useState("");
  const candidates = nodes.usable.filter((n) => n.id !== container.nodeId);
  const choice =
    nodeId ||
    (container.backupNodeId &&
    candidates.some((n) => n.id === container.backupNodeId)
      ? container.backupNodeId
      : candidates[0]?.id) ||
    "";
  const def = useDefaultDir(choice, project.id, project.networkName, container.name);
  const ref = { projectId: project.id, container: container.name, nodeId: choice, dir: dir.trim() || def };

  // 对话框本身就是确认：说明写了后果，提交后直接执行。
  const submit = () => {
    onClose();
    run.run(() => (online ? projectClient.migrateContainer(ref) : projectClient.redeployElsewhere(ref)));
  };

  return (
    <Dialog open onClose={onClose} title={online ? p.migrateTitle : p.redeployTitle} description={online ? p.migrateBody : p.redeployBody} className="max-w-[520px]">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <FormRow>
          <Field label={p.target} htmlFor="move-node">
            <Select id="move-node" value={choice} onChange={(e) => setNodeId(e.target.value)}>
              {candidates.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                  {n.id === container.backupNodeId ? p.backupSuffix : ""}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={p.dir} htmlFor="move-dir" help={t.projectData.dirHelp}>
            <Input id="move-dir" value={dir} onChange={(e) => setDir(e.target.value)} placeholder={def} autoComplete="off" spellCheck={false} />
          </Field>
        </FormRow>
        <DialogActions>
          <Button variant="outline" readOnlyOk onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" variant={online ? "default" : "destructive"} disabled={run.busy || !choice}>
            {run.busy && <Spinner />}
            {online ? p.migrate : p.redeployShort}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// ---- 顶替 ----

function duration(ms: bigint) {
  const s = Math.ceil(Number(ms) / 1000);
  return s >= 60
    ? t.common.time.minutes(Math.ceil(s / 60))
    : t.common.time.seconds(s);
}

const standbyOf = (container: Container) => (container.failover?.a?.nodeId === container.nodeId ? container.failover?.b : container.failover?.a);

// FailoverRow：还没有顶替时的一行。和备份节点互斥，做不了时写原因，没有按钮。
function FailoverRow({ project, container, nodes }: { project: Project; container: Container; nodes: Nodes }) {
  const f = t.projectData.failover;
  const [open, setOpen] = useState(false);
  const standbys = nodes.usable.filter((n) => n.id !== container.nodeId);
  const available = !container.backupNodeId && standbys.length > 0;
  return (
    <>
      <SettingRow label={f.title} help={f.setupHelp} description={container.backupNodeId ? f.hasBackup : standbys.length === 0 ? f.noCandidates : f.unset}>
        {available ? (
          <Button variant="outline" onClick={() => setOpen(true)}>
            {t.projectData.setUp}
          </Button>
        ) : null}
      </SettingRow>
      {open && <FailoverDialog project={project} container={container} nodes={nodes} onClose={() => setOpen(false)} />}
    </>
  );
}

// FailoverDialog：设置顶替。只露节点、见证人和两个开关，目录和租约时长收在「高级设置」。
function FailoverDialog({ project, container, nodes, onClose }: { project: Project; container: Container; nodes: Nodes; onClose: () => void }) {
  const f = t.projectData.failover;
  const { busy, error, call } = useCall(project.id);
  const [advanced, setAdvanced] = useState(false);
  const [form, setForm] = useState({
    standby: "",
    dir: "",
    witness: "",
    lease: "",
    autoStateful: false,
    autoFailback: true,
  });
  const standbys = nodes.usable.filter((n) => n.id !== container.nodeId);
  const standbyId = form.standby || standbys[0]?.id || "";
  const witnesses = nodes.usable.filter((n) => n.id !== container.nodeId && n.id !== standbyId);
  // 目录收起时也要用默认值，所以在这里算，不放进折叠的内容里。
  const def = useDefaultDir(standbyId, project.id, project.networkName, container.name);
  // 租约时长留空用默认值，否则须为正整数。
  const leaseError = form.lease.trim() && !/^[1-9]\d*$/.test(form.lease.trim()) ? f.leaseInvalid : "";
  return (
    <Dialog open onClose={onClose} title={f.set} className="max-w-[520px]">
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          if (leaseError) {
            setAdvanced(true);
            return;
          }
          const ok = await call(() =>
            projectClient.setFailover({
              projectId: project.id,
              container: container.name,
              standbyNodeId: standbyId,
              standbyDir: form.dir.trim() || def,
              witnessNodeId: form.witness,
              leaseSeconds: Number(form.lease.trim() || 0),
              autoStateful: form.autoStateful,
              autoFailback: form.autoFailback,
            }),
          );
          if (ok) onClose();
        }}
      >
        <FormRow>
          <Field label={f.standbyNode} htmlFor="fo-standby">
            <Select
              id="fo-standby"
              value={standbyId}
              // 见证人不能和顶替节点相同。
              onChange={(e) => setForm({ ...form, standby: e.target.value, witness: form.witness === e.target.value ? "" : form.witness })}
            >
              {standbys.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={f.witness} htmlFor="fo-witness" help={f.witnessHelp}>
            <Select id="fo-witness" value={form.witness} onChange={(e) => setForm({ ...form, witness: e.target.value })}>
              <option value="">{t.projectData.thisPanel}</option>
              {witnesses.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </Select>
          </Field>
        </FormRow>
        <div className="grid justify-items-start gap-2">
          <Check checked={form.autoFailback} onChange={(v) => setForm({ ...form, autoFailback: v })} help={f.autoFailbackHelp}>
            {f.autoFailback}
          </Check>
          {container.stateful && (
            <Check checked={form.autoStateful} onChange={(v) => setForm({ ...form, autoStateful: v })} help={f.autoStatefulHelp}>
              {f.autoStateful}
            </Check>
          )}
        </div>
        {/* 高级设置：和添加容器的「更多设置」一样，展开时内容从上方滑入，箭头旋转。 */}
        <button
          type="button"
          aria-expanded={advanced}
          aria-controls="fo-advanced"
          onClick={() => setAdvanced(!advanced)}
          className="inline-flex cursor-pointer items-center gap-1 justify-self-start rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:text-foreground focus-ring"
        >
          <ChevronRight strokeWidth={2.25} className={advanced ? "size-4 rotate-90 transition-transform" : "size-4 transition-transform"} />
          {advanced ? f.advanced : f.advancedHint}
        </button>
        {advanced && (
          <div id="fo-advanced" className="animate-slide-from-above">
            <FormRow>
              <Field label={f.standbyDir} htmlFor="fo-dir" help={t.projectData.dirHelp}>
                <Input id="fo-dir" value={form.dir} onChange={(e) => setForm({ ...form, dir: e.target.value })} placeholder={def} autoComplete="off" spellCheck={false} />
              </Field>
              <Field label={f.leaseSeconds} htmlFor="fo-lease" help={f.leaseHelp} error={leaseError}>
                <Input id="fo-lease" value={form.lease} onChange={(e) => setForm({ ...form, lease: e.target.value })} placeholder="30" inputMode="numeric" />
              </Field>
            </FormRow>
          </div>
        )}
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          <Button variant="outline" readOnlyOk onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy || !standbyId}>
            {busy && <Spinner />}
            {f.set}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// TakeoverNotice：有状态容器等待确认顶替，放在页面最上面。
function TakeoverNotice({ project, container, nodes }: { project: Project; container: Container; nodes: Nodes }) {
  const f = t.projectData.failover;
  const { busy, error, call } = useCall(project.id);
  const standby = standbyOf(container);
  return (
    <div className="grid gap-2">
      <Notice
        tone="warning"
        title={f.pendingTitle}
        actions={
          <Button
            disabled={busy}
            onClick={async () =>
              (await confirmAction(f.confirmTakeoverPrompt(standby ? nodes.name(standby.nodeId, standby.name) : "", container.name), { action: f.confirmTakeover, danger: true })) &&
              call(() => projectClient.confirmTakeover({ projectId: project.id, container: container.name }))
            }
          >
            {busy && <Spinner />}
            {f.confirmTakeover}
          </Button>
        }
      >
        {f.pendingBody(nodes.name(container.nodeId))}
      </Notice>
      {error && <Alert>{error}</Alert>}
    </div>
  );
}

// FailoverDetail：已设置顶替：标签值列表，取消在标题栏，见证人旁边是更换。
function FailoverDetail({ project, container, nodes }: { project: Project; container: Container; nodes: Nodes }) {
  const f = t.projectData.failover;
  const { busy, error, call } = useCall(project.id);
  const [changingWitness, setChangingWitness] = useState(false);
  const cfg = container.failover!;
  const holder = container.failoverHolder;
  const standby = container.failoverStandby;
  const ref = { projectId: project.id, container: container.name };

  const holderOnline = nodes.online(container.nodeId);
  // 持有者上报的是最近一次同步给热备的时间，持有者离线时用热备记录的最近一次收到同步的时间。
  const lastSync = holder?.lastSync ?? standby?.lastSync;
  const standbyNode = standbyOf(container);
  let state: React.ReactNode = <Status tone="neutral">—</Status>;
  if (holder?.failingBack)
    state = (
      <Status tone="info" pulse>
        {f.failingBack}
      </Status>
    );
  else if (holder?.role === "holder" && holder.leaseRemainingMs > 0n)
    state = <Status tone="success">{t.common.status.running}</Status>;
  else if (holder?.role === "holder")
    state = <Status tone="danger">{f.leaseLost}</Status>;
  else if (!holderOnline && standby?.pendingConfirm)
    state = <Status tone="warning">{f.awaitingConfirm(nodes.name(container.nodeId))}</Status>;
  else if (!holderOnline)
    state = <Status tone="warning">{f.holderOffline(nodes.name(container.nodeId))}</Status>;

  return (
    <Section
      title={f.title}
      help={f.setupHelp}
      actions={
        <Button
          variant="outline"
          disabled={busy}
          onClick={async () =>
            (await confirmAction(f.clearConfirm(container.name, nodes.name(container.nodeId)), { action: f.clear, danger: true })) && call(() => projectClient.clearFailover(ref))
          }
        >
          {f.clear}
        </Button>
      }
    >
      <KeyValue
        items={[
          // 等待确认顶替时最上面已有提示，这里不再重复状态。
          ...(standby?.pendingConfirm ? [] : ([[t.projectData.state, state]] as [React.ReactNode, React.ReactNode][])),
          [
            f.runningNode,
            <span>
              {nodes.name(container.nodeId)}{" "}
              {cfg.a?.nodeId === container.nodeId ? <span className="text-muted-foreground">{holderOnline ? f.originalNode : f.originalNodeLost}</span> : <span className="text-warning-text">{f.failedOver}</span>}
            </span>,
          ],
          [
            f.standbyNode,
            <span>
              {standbyNode && nodes.name(standbyNode.nodeId, standbyNode.name)}
              {standbyNode && !nodes.online(standbyNode.nodeId) && <span className="text-muted-foreground"> {t.common.status.offline}</span>}
            </span>,
          ],
          [
            f.witness,
            <span>
              {!cfg.witnessIsPanel ? (
                cfg.witness && nodes.name(cfg.witness.nodeId, cfg.witness.name)
              ) : container.failoverWitnessSelf ? (
                t.projectData.thisPanel
              ) : (
                <span className="text-warning-text" title={f.oldPanelHint}>
                  {f.oldPanel}
                </span>
              )}
              <Button variant="ghost" size="row" disabled={busy} className="ml-1" onClick={() => setChangingWitness(true)}>
                {f.change}
              </Button>
            </span>,
          ],
          [t.projectData.lastSync, <span title={formatDateTime(toDate(lastSync))}>{lastSync ? capitalize(timeAgo(toDate(lastSync))) : f.notSynced}</span>],
          [f.lease, <span title={holder?.epoch ? f.epochTitle(holder.epoch) : undefined}>{t.common.time.seconds(cfg.leaseSeconds || 30)}</span>],
          [
            f.autoFailbackLabel,
            <span>
              {cfg.autoFailback ? f.on : f.off}
              {holder && holder.failbackInMs > 0n && ` · ${f.failbackIn(duration(holder.failbackInMs))}`}
            </span>,
          ],
        ]}
      />
      {(holder?.lastError || standby?.lastError) && (
        <Notice tone="danger" title={t.projectData.backup.syncFailed}>
          {holder?.lastError || standby?.lastError}
        </Notice>
      )}
      {error && <Alert>{error}</Alert>}
      {changingWitness && <WitnessDialog project={project} container={container} nodes={nodes} onClose={() => setChangingWitness(false)} />}
    </Section>
  );
}

function WitnessDialog({ project, container, nodes, onClose }: { project: Project; container: Container; nodes: Nodes; onClose: () => void }) {
  const f = t.projectData.failover;
  const { busy, error, call } = useCall(project.id);
  const [witness, setWitness] = useState<string>();
  const cfg = container.failover!;
  const options = nodes.usable.filter((n) => n.id !== cfg.a?.nodeId && n.id !== cfg.b?.nodeId && (cfg.witnessIsPanel || n.id !== cfg.witness?.nodeId));
  // 本面板已是见证人时不再列出它。默认选第一项，不让「请选择」挡住提交；一项都没有时确实换不了。
  const panelOption = !(cfg.witnessIsPanel && container.failoverWitnessSelf);
  const choices = [...(panelOption ? [""] : []), ...options.map((n) => n.id)];
  const choice = witness ?? choices[0];
  return (
    <Dialog open onClose={onClose} title={f.changeWitness}>
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          if (choice === undefined) return;
          if (await call(() => projectClient.changeWitness({ projectId: project.id, container: container.name, witnessNodeId: choice }))) onClose();
        }}
      >
        <Field label={f.witness} htmlFor="fo-change-witness" help={f.witnessHelp} hint={choice === undefined ? f.noWitnessCandidates : undefined}>
          <Select id="fo-change-witness" value={choice ?? ""} disabled={choice === undefined} onChange={(e) => setWitness(e.target.value)}>
            {panelOption && <option value="">{t.projectData.thisPanel}</option>}
            {options.map((n) => (
              <option key={n.id} value={n.id}>
                {n.name}
              </option>
            ))}
          </Select>
        </Field>
        {error && <Alert>{error}</Alert>}
        <DialogActions>
          <Button variant="outline" readOnlyOk onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" disabled={busy || choice === undefined}>
            {busy && <Spinner />}
            {f.change}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
