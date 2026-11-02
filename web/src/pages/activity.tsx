// 活动记录：用户在面板上的操作和节点、面板发出的告警。
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { Alert, Button, EmptyRow, Loading, Segmented, Spinner, Status, Table, Td, Th } from "@/components/ui";
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

export function ActivityCard() {
  const kind = useSearch({ from: "/app/settings" }).kind ?? "all";
  const navigate = useNavigate({ from: "/settings" });
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
    <div className="grid gap-4">
      <div>
        <Segmented
          value={kind}
          onChange={setKind}
          items={[
            { value: "all", label: t.activity.all },
            { value: "operation", label: t.activity.operations },
            { value: "alert", label: t.activity.alerts },
          ]}
        />
      </div>
      {list.isPending ? (
        <Loading />
      ) : list.error ? (
        <Alert>{errorMessage(list.error)}</Alert>
      ) : entries.length === 0 ? (
        <EmptyRow>{t.activity.empty}</EmptyRow>
      ) : (
        <Table compact>
          <thead>
            <tr>
              <Th>{t.activity.time}</Th>
              <Th>{t.activity.actor}</Th>
              <Th>{t.activity.action}</Th>
              <Th className="hidden md:table-cell">{t.activity.target}</Th>
              <Th>{t.activity.result}</Th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <tr key={String(e.id)}>
                <Td className="text-[13px] whitespace-nowrap text-muted-foreground" title={formatDateTime(toDate(e.time))}>
                  {timeAgo(toDate(e.time))}
                </Td>
                <Td className="text-[13px]">{e.actor || "—"}</Td>
                <Td className="text-[13px]">{actionLabel(e)}</Td>
                <Td className="hidden font-mono text-xs text-muted-foreground md:table-cell">
                  {e.projectId ? `${projectName(e.projectId)}${e.target ? ` / ${e.target}` : ""}` : e.target || "—"}
                </Td>
                <Td className="max-w-72 text-[13px]">
                  {e.kind === "alert" ? (
                    <Status tone={e.action === "resolved" ? "success" : "warning"}>
                      <span className="whitespace-normal">{e.detail}</span>
                    </Status>
                  ) : e.failed ? (
                    <span title={e.detail}>
                      <Status tone="danger">{t.activity.failed}</Status>
                    </span>
                  ) : (
                    <span className="flex min-w-0 items-center gap-2">
                      <Status tone="success">{t.activity.succeeded}</Status>
                      {e.detail && (
                        <span className="truncate font-mono text-xs text-muted-foreground" title={e.detail}>
                          {e.detail}
                        </span>
                      )}
                    </span>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {list.hasNextPage && (
        <div className="flex justify-center">
          <Button readOnlyOk size="sm" variant="ghost" disabled={list.isFetchingNextPage} onClick={() => list.fetchNextPage()}>
            {list.isFetchingNextPage && <Spinner />}
            {t.activity.more}
          </Button>
        </div>
      )}
    </div>
  );
}
