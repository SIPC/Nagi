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
import { authClient, isUnauthenticated, setupClient } from "@/lib/api";
import { LoginPage, SetupPage } from "@/pages/auth";
import { AppLayout } from "@/pages/layout";
import { NewProjectPage, ProjectsPage } from "@/pages/projects";
import { ProjectPage, ProjectSplit, projectTabs, type ProjectTab } from "@/pages/project";
import { ContainerPage, containerTabs, type DetailView } from "@/pages/project-containers";
import { NodePage, NodesPage, nodeTabs, type NodeTab, type NodesView } from "@/pages/nodes";
import { SettingsPage, settingsTabs, type SettingsTab } from "@/pages/settings";
import { adoptAccountLocale } from "@/i18n";
import "./index.css";

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
  defaultOptions: { queries: { retry: (count, err) => !isUnauthenticated(err) && count < 2 } },
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
    throw redirect({ to: "/projects" });
  },
});

const projectsRoute = createRoute({ getParentRoute: () => appRoute, path: "/projects", component: ProjectsPage });

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
    return (
      <ProjectSplit key={projectId} projectId={projectId} tab={tab ?? "containers"} container={child?.params.container} view={child?.search.view}>
        {child ? (
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
  validateSearch: (search: Record<string, unknown>): { view?: DetailView } => ({
    view: containerTabs.includes(search.view as DetailView) ? (search.view as DetailView) : undefined,
  }),
  component: function ContainerRoute() {
    const { projectId, container } = containerRoute.useParams();
    const { view } = containerRoute.useSearch();
    const navigate = containerRoute.useNavigate();
    return (
      <ContainerPage
        key={projectId + "/" + container}
        projectId={projectId}
        name={container}
        view={view ?? "overview"}
        onView={(t) => navigate({ search: { view: t === "overview" ? undefined : t }, replace: true })}
      />
    );
  },
});

const nodesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/nodes",
  validateSearch: (search: Record<string, unknown>): { view?: NodesView } => ({
    view: search.view === "map" ? "map" : undefined,
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
  validateSearch: (search: Record<string, unknown>): { tab?: NodeTab } => ({
    tab: nodeTabs.some((t) => t.value === search.tab) ? (search.tab as NodeTab) : undefined,
  }),
  component: function Node() {
    const { nodeId } = nodeRoute.useParams();
    const { tab } = nodeRoute.useSearch();
    const navigate = nodeRoute.useNavigate();
    return (
      <NodePage
        key={nodeId}
        nodeId={nodeId}
        tab={tab ?? "metrics"}
        onTab={(t) => navigate({ search: { tab: t === "metrics" ? undefined : t }, replace: true })}
      />
    );
  },
});

const settingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/settings",
  validateSearch: (search: Record<string, unknown>): { tab?: SettingsTab } => ({
    tab: settingsTabs.some((t) => t.value === search.tab) ? (search.tab as SettingsTab) : undefined,
  }),
  component: function Settings() {
    const { tab } = settingsRoute.useSearch();
    const navigate = settingsRoute.useNavigate();
    return <SettingsPage tab={tab ?? "access"} onTab={(t) => navigate({ search: { tab: t === "access" ? undefined : t }, replace: true })} />;
  },
});

const routeTree = rootRoute.addChildren([
  setupRoute,
  loginRoute,
  appRoute.addChildren([indexRoute, projectsRoute, newProjectRoute, projectRoute.addChildren([containerRoute]), nodesRoute, nodeRoute, settingsRoute]),
]);

const router = createRouter({ routeTree, context: { queryClient }, defaultPreload: "intent" });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
