// 日志：用户在面板上的操作和节点、面板发出的告警。只有管理员能看（接口也会拒绝）。
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Navigate, useNavigate, useSearch } from "@tanstack/react-router";
import { Button, EmptyRow, List, QueryError, Row, RowsSkeleton, Segmented, Slide, Spinner, Status } from "@/components/ui";
import { PageHeader } from "@/pages/layout";
import { useRole } from "@/components/role";
import type { ActivityEntry } from "@/gen/nagi/v1/api_pb";
import { errorMessage, projectClient, settingsClient, toDate } from "@/lib/api";
import { formatDateTime, timeAgo } from "@/lib/utils";
import { t } from "@/i18n";

const pageSize = 50;

// 接口方法名换成给人看的动作；没有登记的显示方法名。
export function actionLabel(e: ActivityEntry) {
  if (e.kind === "alert") return e.action === "resolved" ? t.activity.resolved : t.activity.firing;
  return (t.activity.actions as Record<string, string>)[e.action] ?? e.action;
}

function result(e: ActivityEntry) {
  if (e.kind === "alert")
    return e.action === "resolved" ? (
      <Status tone="success">{actionLabel(e)}</Status>
    ) : (
      <span className="text-warning-text">
        <Status tone="warning">{actionLabel(e)}</Status>
      </span>
    );
  if (e.failed)
    return (
      <span className="text-destructive">
        <Status tone="danger">{t.activity.failed}</Status>
      </span>
    );
  return <Status tone="success">{t.activity.succeeded}</Status>;
}

export function ActivityPage() {
  const { isAdmin } = useRole();
  if (!isAdmin) return <Navigate to="/overview" replace />;
  return <ActivityList />;
}

function ActivityList() {
  const kind = useSearch({ from: "/app/activity" }).kind ?? "all";
  const navigate = useNavigate({ from: "/activity" });
  const setKind = (v: "all" | "operation" | "alert") => navigate({ search: (s) => ({ ...s, kind: v === "all" ? undefined : v }), replace: true });
  const list = useInfiniteQuery({
    queryKey: ["activity", kind],
    initialPageParam: 0n,
    queryFn: ({ pageParam }) => settingsClient.listActivity({ kind: kind === "all" ? "" : kind, before: pageParam, limit: pageSize }),
    getNextPageParam: (last) => (last.entries.length < pageSize ? undefined : last.entries[last.entries.length - 1].id),
    refetchInterval: 15000,
  });
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}) });
  const projectName = (id: string) => projects.data?.projects.find((s) => s.project?.id === id)?.project?.name ?? id;
  const entries = list.data?.pages.flatMap((p) => p.entries) ?? [];

  return (
    <>
    <PageHeader
      title={t.app.nav.logs}
      description={t.activity.description}
      actions={
        <Segmented
          value={kind}
          onChange={setKind}
          items={[
            { value: "all", label: t.activity.all },
            { value: "operation", label: t.activity.operations },
            { value: "alert", label: t.activity.alerts },
          ]}
        />
      }
    />
    <div className="grid gap-4">
      <Slide id={kind} index={["all", "operation", "alert"].indexOf(kind)} inner="grid gap-4 max-md:gap-2">
      {list.isPending ? (
        <RowsSkeleton count={5} />
      ) : (
        <>
          {/* 定时刷新或加载更多失败时，已经显示的记录留着。 */}
          {list.error && <QueryError error={errorMessage(list.error)} onRetry={() => list.refetch()} />}
          {list.data &&
            (entries.length === 0 ? (
              <EmptyRow>{t.activity.empty}</EmptyRow>
            ) : (
              <List>
                {entries.map((e) => {
                  const target = e.projectId ? `${projectName(e.projectId)}${e.target ? ` / ${e.target}` : ""}` : e.target;
                  const name = (e.kind === "alert" && e.detail) || actionLabel(e);
                  const time = timeAgo(toDate(e.time));
                  // 告警第二行写类型和是否已恢复，操作写谁做的和失败原因。
                  const parts = e.kind === "alert" ? [t.activity.firing, e.action === "resolved" && t.activity.resolved, time] : [e.actor, e.via && t.activity.via(e.via.replace(/^mcp:/, "")), e.detail, time];
                  return (
                    <Row
                      key={String(e.id)}
                      title={
                        <span className="min-w-0 truncate" title={e.kind === "alert" ? [e.detail, e.actor].filter(Boolean).join(" · ") : undefined}>
                          {target ? `${name} ${target}` : name}
                        </span>
                      }
                      sub={
                        <span className="wrap-anywhere" title={formatDateTime(toDate(e.time))}>
                          {parts.filter(Boolean).join(" · ")}
                        </span>
                      }
                      cols={[{ content: result(e), phone: true }]}
                    />
                  );
                })}
              </List>
            ))}
        </>
      )}
      </Slide>
      {list.hasNextPage && (
        <div>
          <Button readOnlyOk variant="ghost" className="-ml-3" disabled={list.isFetchingNextPage} onClick={() => list.fetchNextPage()}>
            {list.isFetchingNextPage && <Spinner />}
            {t.activity.more}
          </Button>
        </div>
      )}
    </div>
    </>
  );
}
