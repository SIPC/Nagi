import type zh from "../zh/project";

export default {
  sections: {
    overview: "Overview",
    network: "Network",
    settings: "Settings",
  },
  breadcrumb: "Projects",
  nodes: (n, names) => (n === 1 ? `Node ${names}` : `Nodes ${names}`),
  noContainers: "No containers",
  rebuiltHint:
    "This project was rebuilt from node caches after the panel was lost or replaced. The original creation time, version history and deployment history are lost.",
  rebuiltAt: (ago) => `Rebuilt from node cache ${ago}`,
  createdAt: (ago) => `Created ${ago}`,
  containerCount: (n) => (n === 1 ? "1 container" : `${n} containers`),
  groupCount: (n) => (n === 1 ? "1 container group" : `${n} container groups`),
  statefulCount: (n) => `${n} stateful`,
  column: {
    allProjects: "All projects",
    project: "Project",
    containers: "Containers",
    draft: "Draft",
    stateOnNode: (state, node) => ` (${state}, node ${node})`,
  },
} satisfies typeof zh;
