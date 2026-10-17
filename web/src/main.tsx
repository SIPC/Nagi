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
} from "@tanstack/react-router";
import { authClient, isUnauthenticated, setupClient } from "@/lib/api";
import { LoginPage, SetupPage } from "@/pages/auth";
import { AppLayout } from "@/pages/layout";
import { NewProjectPage, ProjectsPage } from "@/pages/projects";
import { ProjectPage } from "@/pages/project";
import { NodesPage } from "@/pages/nodes";
import "./index.css";

// 任何接口返回“未登录”时回到登录页。
const onError = (err: unknown) => {
  if (isUnauthenticated(err) && router.state.location.pathname !== "/login") {
    router.navigate({ to: "/login" });
  }
};

const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError }),
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
      await context.queryClient.fetchQuery({ queryKey: ["me"], queryFn: () => authClient.me({}), staleTime: 60_000 });
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

const projectRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/projects/$nodeId/$projectId",
  component: function Project() {
    const { nodeId, projectId } = projectRoute.useParams();
    return <ProjectPage key={`${nodeId}/${projectId}`} nodeId={nodeId} projectId={projectId} />;
  },
});

const nodesRoute = createRoute({ getParentRoute: () => appRoute, path: "/nodes", component: NodesPage });

const routeTree = rootRoute.addChildren([
  setupRoute,
  loginRoute,
  appRoute.addChildren([indexRoute, projectsRoute, newProjectRoute, projectRoute, nodesRoute]),
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
