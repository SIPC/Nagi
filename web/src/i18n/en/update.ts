import type zh from "../zh/update";

export default {
  badge: "Update available",
  current: "Current version",
  latest: "Latest version",
  check: "Check for updates",
  upToDate: "Up to date",
  availableTitle: (version: string) => `${version} available`,
  help: "The panel downloads the new version from GitHub Release (or the mirror set during installation), verifies it and restarts in place. The page is interrupted for a few seconds; project containers keep running.",
  notes: "View release notes",
  primaryHint: "Standby panels are updated separately.",
  primaryHelp: "If their database version differs from this panel's, they pause syncing until both are on the same version.",
  standbyHint: "Only this standby panel is updated.",
  standbyHelp: "If its database version differs from the primary's, syncing pauses until both are on the same version.",
  action: "Update",
  updatePanel: "Update panel",
  confirm: (version: string) =>
    `Update the panel to ${version}? The panel downloads and verifies the new version, then restarts in place. Operations in progress, such as deployments, are interrupted. You can't go back to an older version from the panel afterwards.`,
  restartingTitle: (version: string) => `Replaced with ${version}. The panel is restarting`,
  restartingText: "The page refreshes automatically once the panel is back.",
  retry: "Retry",
  installingTitle: (version) => `Updating to ${version}`,
  installingText: "The panel is downloading and verifying the new version, then restarts in place and this page refreshes. You can leave this page; progress is kept on the panel.",
  tips: {
    view: "View",
    available: (version) => `New version ${version} available`,
    panelFailed: (version) => `Panel update to ${version} failed`,
    nodesRunning: (n) => `Upgrading nodes, ${n} left`,
    nodeFailed: (name) => `Upgrade of node ${name} failed`,
    nodesAvailable: (n, version) => `${n} ${n === 1 ? "node" : "nodes"} can be upgraded to ${version}`,
  },
} satisfies typeof zh;
