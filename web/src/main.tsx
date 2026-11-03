import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  Outlet,
  redirect,
  RouterProvider,
  useMatch,
} from "@tanstack/react-router";
import { authClient, isNotFound, isUnauthenticated, setupClient } from "@/lib/api";
import { LoginPage, SetupPage } from "@/pages/auth";
import { OAuthAuthorizePage } from "@/pages/tokens";
import { AppLayout, NotFound } from "@/pages/layout";
import { OverviewPage } from "@/pages/overview";
import { metricRanges, type MetricRange } from "@/components/metrics";
import { NewProjectPage, ProjectsPage } from "@/pages/projects";
import { ProjectPage, ProjectSplit, projectTabs, type ProjectTab } from "@/pages/project";
import { AddContainerPage, ContainerPage, containerTabs, type DetailView } from "@/pages/project-containers";
import { NodePage, NodesPage, nodeTabs, type NodeTab, type NodesView } from "@/pages/nodes";
import { SettingsPage, settingsTabs, type SettingsTab } from "@/pages/settings";
import { ActivityPage } from "@/pages/activity";
import { adoptAccountLocale } from "@/i18n";
import "./index.css";
import { ConfirmHost } from "@/components/confirm";
import { ElevateHost } from "@/components/elevate";
import { Toaster } from "@/components/ui/sonner";

// 任何接口返回“未登录”时回到登录页。进入页面时的登录检查（["me"]）不算：
// 它由 appRoute 处理，未初始化时要去初始化页而不是登录页。
const onError = (err: unknown) => {
  const path = router.state.location.pathname;
  if (isUnauthenticated(err) && path !== "/login" && path !== "/setup") {
    router.navigate({ to: "/login" });
  }
};

const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError: (err, query) => query.queryKey[0] !== "me" && onError(err) }),
  mutationCache: new MutationCache({ onError }),
  defaultOptions: { queries: { retry: (count, err) => !isUnauthenticated(err) && !isNotFound(err) && count < 2 } },
});

const rootRoute = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: Outlet,
});

const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/setup",
  validateSearch: (search: Record<string, unknown>) => ({ token: typeof search.token === "string" ? search.token : "" }),
  component: function Setup() {
    const { token } = setupRoute.useSearch();
    return <SetupPage token={token} />;
  },
});

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: LoginPage,
});

// OAuth 授权页（见 docs/mcp.md#oauth）：不在导航框里，没登录时自己转去登录页并记下回来的地址。
const oauthRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/oauth/authorize",
  component: OAuthAuthorizePage,
});

// 需要登录的页面。未初始化时去初始化页，未登录时去登录页。
const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "app",
  beforeLoad: async ({ context }) => {
    try {
      const me = await context.queryClient.fetchQuery({ queryKey: ["me"], queryFn: () => authClient.me({}), staleTime: 60_000 });
      adoptAccountLocale(me.language);
    } catch (err) {
      if (!isUnauthenticated(err)) throw err;
      const state = await setupClient.getSetupState({});
      throw redirect({ to: state.initialized ? "/login" : "/setup", search: state.initialized ? undefined : { token: "" } });
    }
  },
  component: AppLayout,
});

const indexRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/overview" });
  },
});

// 全局概览。桑基图的视图和趋势的时间范围写进地址。
const overviewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/overview",
  validateSearch: (search: Record<string, unknown>): { flow?: "ext" | "mesh"; range?: MetricRange } => ({
    flow: search.flow === "ext" || search.flow === "mesh" ? search.flow : undefined,
    range: metricRanges.some((r) => r.value === search.range && r.value !== "1h") ? (search.range as MetricRange) : undefined,
  }),
  component: OverviewPage,
});

const projectsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/projects",
  validateSearch: (search: Record<string, unknown>): { q?: string; sort?: "name" } => ({
    q: typeof search.q === "string" && search.q ? search.q : undefined,
    sort: search.sort === "name" ? "name" : undefined,
  }),
  component: ProjectsPage,
});

const newProjectRoute = createRoute({ getParentRoute: () => appRoute, path: "/projects/new", component: NewProjectPage });

// 项目页。宽屏左边是项目栏，右边是概览、网络、设置之一，或者打开的容器（子路由 c/$container）。
const projectRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/projects/$projectId",
  validateSearch: (search: Record<string, unknown>): { tab?: ProjectTab } => ({
    tab: projectTabs.includes(search.tab as ProjectTab) ? (search.tab as ProjectTab) : undefined,
  }),
  component: function Project() {
    const { projectId } = projectRoute.useParams();
    const { tab } = projectRoute.useSearch();
    const navigate = projectRoute.useNavigate();
    const child = useMatch({ from: containerRoute.id, shouldThrow: false });
    const adding = useMatch({ from: addContainerRoute.id, shouldThrow: false });
    return (
      <ProjectSplit key={projectId} projectId={projectId} tab={tab ?? "containers"} container={child?.params.container} view={child?.search.view} adding={!!adding}>
        {child || adding ? (
          <Outlet />
        ) : (
          <ProjectPage
            projectId={projectId}
            tab={tab ?? "containers"}
            onTab={(t) => navigate({ search: { tab: t === "containers" ? undefined : t }, replace: true })}
          />
        )}
      </ProjectSplit>
    );
  },
});

// 容器页的标签记在 view 参数里（tab 是上层项目页的）。
const containerRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "c/$container",
  validateSearch: (search: Record<string, unknown>): { view?: DetailView; volume?: string; deploy?: boolean } => ({
    // 旧地址的「数据」标签拆成了存储和高可用，快照在存储里。
    view: search.view === "data" ? "storage" : containerTabs.includes(search.view as DetailView) ? (search.view as DetailView) : undefined,
    // 存储标签里打开的卷（文件管理就地显示，后退能回到卷列表）。
    volume: typeof search.volume === "string" && search.volume ? search.volume : undefined,
    // 添加容器时选了「添加并部署」：进入容器页后开始部署。
    deploy: search.deploy === true ? true : undefined,
  }),
  component: function ContainerRoute() {
    const { projectId, container } = containerRoute.useParams();
    const { view, deploy } = containerRoute.useSearch();
    const navigate = containerRoute.useNavigate();
    return (
      <ContainerPage
        key={projectId + "/" + container}
        projectId={projectId}
        name={container}
        view={view ?? "overview"}
        onView={(t) => navigate({ search: { view: t === "overview" ? undefined : t }, replace: true })}
        autoDeploy={!!deploy}
        onAutoDeployed={() => navigate({ search: (s) => ({ ...s, deploy: undefined }), replace: true })}
      />
    );
  },
});

// 添加容器占整个主区，不和项目概览混在一页。
const addContainerRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: "add",
  component: function AddContainerRoute() {
    const { projectId } = addContainerRoute.useParams();
    return <AddContainerPage key={projectId} projectId={projectId} />;
  },
});

const nodesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/nodes",
  validateSearch: (search: Record<string, unknown>): { view?: NodesView } => ({
    view: search.view === "network" || search.view === "ha" ? search.view : undefined,
  }),
  component: function Nodes() {
    const { view } = nodesRoute.useSearch();
    const navigate = nodesRoute.useNavigate();
    return <NodesPage view={view ?? "cards"} onView={(v) => navigate({ search: { view: v === "cards" ? undefined : v }, replace: true })} />;
  },
});

const nodeRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/nodes/$nodeId",
  validateSearch: (search: Record<string, unknown>): { tab?: NodeTab; path?: string } => ({
    tab: nodeTabs.some((t) => t.value === search.tab) ? (search.tab as NodeTab) : undefined,
    path: typeof search.path === "string" && search.path.startsWith("/") ? search.path : undefined,
  }),
  component: function Node() {
    const { nodeId } = nodeRoute.useParams();
    const { tab, path } = nodeRoute.useSearch();
    const navigate = nodeRoute.useNavigate();
    return (
      <NodePage
        key={nodeId}
        nodeId={nodeId}
        tab={tab ?? "metrics"}
        path={path}
        onTab={(t) => navigate({ search: { tab: t === "metrics" ? undefined : t, path: t === "files" || t === "terminal" ? path : undefined }, replace: true })}
        // 进目录是一次导航，浏览器后退能回到上一层。
        onFilePath={(p) => navigate({ search: { tab: "files", path: p } })}
        onOpenTerminal={(p) => navigate({ search: { tab: "terminal", path: p } })}
      />
    );
  },
});

const settingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/settings",
  validateSearch: (search: Record<string, unknown>): { tab?: SettingsTab; activity?: "" | "operation" | "alert" } => ({
    tab: settingsTabs.some((t) => t.value === search.tab) ? (search.tab as SettingsTab) : undefined,
    // 日志以前是设置里的一项（?tab=activity），旧地址由 beforeLoad 跳到日志页。路由会拿结果再校验一次，所以 activity 本身也要认。
    activity: search.tab === "activity" ? (search.kind === "operation" || search.kind === "alert" ? search.kind : "") : search.activity === "" || search.activity === "operation" || search.activity === "alert" ? search.activity : undefined,
  }),
  beforeLoad: ({ search }) => {
    if (search.activity !== undefined) throw redirect({ to: "/activity", search: { kind: search.activity || undefined }, replace: true });
  },
  component: function Settings() {
    const { tab } = settingsRoute.useSearch();
    return <SettingsPage tab={tab ?? "access"} />;
  },
});

const activityRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/activity",
  validateSearch: (search: Record<string, unknown>): { kind?: "operation" | "alert" } => ({
    kind: search.kind === "operation" || search.kind === "alert" ? search.kind : undefined,
  }),
  component: ActivityPage,
});

const routeTree = rootRoute.addChildren([
  setupRoute,
  loginRoute,
  oauthRoute,
  appRoute.addChildren([indexRoute, overviewRoute, projectsRoute, newProjectRoute, projectRoute.addChildren([containerRoute, addContainerRoute]), nodesRoute, nodeRoute, activityRoute, settingsRoute]),
]);

const router = createRouter({ routeTree, context: { queryClient }, defaultPreload: "intent", defaultNotFoundComponent: NotFound });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <ConfirmHost />
      <ElevateHost />
      <Toaster />
    </QueryClientProvider>
  </StrictMode>,
);
