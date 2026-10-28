import type zh from "../zh/project";

export default {
  sections: {
    overview: "Overview",
    network: "Network",
    settings: "Settings",
  },
  tabs: {
    containers: "Containers",
    network: "Network",
    settings: "Settings",
  },
  breadcrumb: "Projects",
  stateful: "Stateful",
  nodes: (names) => (names.length === 1 ? `Node ${names[0]}` : `Nodes ${names.join(", ")}`),
  noContainers: "No containers",
  rebuiltHint:
    "This project was rebuilt from node caches after the panel was lost or replaced. The original creation time, version history and deployment history are lost.",
  rebuiltAt: (ago) => `Rebuilt from node cache ${ago}`,
  createdAt: (ago) => `Created ${ago}`,
  containerCount: (n) => (n === 1 ? "1 container" : `${n} containers`),
  groupCount: (n) => (n === 1 ? "1 container group" : `${n} container groups`),
  countSeparator: ", ",
  column: {
    containers: "Containers",
    nodeOffline: "Node offline",
    draft: "Draft",
    stateOnNode: (state) => `${state}, node`,
  },
} satisfies typeof zh;
