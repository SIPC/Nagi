import type zh from "../zh/app";

export default {
  nav: {
    overview: "Overview",
    projects: "Projects",
    nodes: "Nodes",
    settings: "Settings",
    open: "Open navigation",
    close: "Close navigation",
  },
  notFound: {
    title: "Page not found",
    body: "The address may be wrong, or the page has been removed.",
    back: "Back to projects",
  },
  names: {
    invalid: "Use lowercase letters, digits and -. It cannot start or end with - and must be at most 32 characters",
    invalidEnv: (key) => `Invalid variable name "${key}": use letters, digits and underscores, and do not start with a digit`,
  },
  nodesOffline: (n) => (n === 1 ? "1 node offline" : `${n} nodes offline`),
  skipToContent: "Skip to main content",
  search: {
    button: "Search",
    placeholder: "Search projects, containers, nodes and settings",
    empty: "No results",
    containers: "Containers",
    newProject: "New project",
    pages: "Pages",
  },
  theme: {
    label: "Theme",
    system: "System",
    light: "Light",
    dark: "Dark",
  },
  logout: "Sign out",
  standby: {
    title: "Standby panel",
    readOnly: "Read-only. Make changes on the primary panel",
    synced: "Synced",
    syncedAgo: (ago) => `Synced (${ago})`,
    disconnected: "Not connected to the primary panel",
    disconnectedAgo: (ago) => `Not connected to the primary panel, last synced ${ago}`,
  },
  terminal: {
    connecting: "Connecting…",
    exited: (code) => `Process exited (exit code ${code})`,
    closed: "Connection closed",
    intro: "Open a shell in the container.",
    noRunning: "No running containers.",
    connect: "Connect",
    reconnect: "Reconnect",
  },
  ports: {
    badFormat: (p) => `"${p}" is invalid. Use node port:container port, e.g. 8080:80 or 53:53/udp`,
    outOfRange: (p) => `"${p}": ports must be between 1 and 65535`,
    reserved: (published, target) =>
      `Node port ${published} is reserved for domain access. Use another port for websites (e.g. 8080:${target}), or bind a domain under Network after deploying`,
  },
} satisfies typeof zh;
