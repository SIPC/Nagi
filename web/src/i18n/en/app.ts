import type zh from "../zh/app";

export default {
  nav: {
    overview: "Overview",
    projects: "Projects",
    nodes: "Nodes",
    activity: "Activity",
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
    invalid: "Use lowercase letters, digits and -. It can't start or end with - and must be at most 32 characters",
    invalidEnv: (key) => `Variable name "${key}" can only contain letters, digits and underscores, and can't start with a digit`,
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
    badFormat: (p) => `"${p}" must be node port:container port, for example 8080:80 or 53:53/udp`,
    outOfRange: (p) => `Ports in "${p}" must be between 1 and 65535`,
    reserved: (published, target) =>
      `Node port ${published} is reserved for domain access. Use another port for websites (for example 8080:${target}), or bind a domain on the Network tab after deploying`,
  },
} satisfies typeof zh;
