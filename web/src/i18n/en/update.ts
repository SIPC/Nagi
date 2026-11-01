import type zh from "../zh/update";

export default {
  badge: "Update available",
  current: "Current version",
  latest: "Latest version",
  check: "Check for updates",
  upToDate: "You're on the latest version.",
  availableTitle: (version: string) => `${version} is available`,
  text: "The panel downloads the new version from GitHub Release (or the mirror set during installation), verifies it and restarts in place. The page is interrupted for a few seconds; project containers keep running.",
  notes: "View release notes",
  primaryHint: "Standby panels are updated separately. If their database version differs from this panel's, they pause syncing until both are on the same version.",
  standbyHint: "This updates only this standby panel. If its database version differs from the primary's, syncing pauses until both are on the same version.",
  action: "Update",
  running: "Downloading…",
  confirm: (version: string) =>
    `Update the panel to ${version}? The panel downloads and verifies the new version, then restarts in place. Operations in progress, such as deployments, are interrupted. You can't go back to an older version from the panel afterwards.`,
  restartingTitle: (version: string) => `Replaced with ${version}. The panel is restarting`,
  restartingText: "The page refreshes automatically once the panel is back. You can then upgrade your nodes on the Nodes page.",
  timeout: (version: string) => `The panel was replaced with ${version} but did not come back within 3 minutes. Check the logs on the server: journalctl -u nagi-server -n 50`,
} satisfies typeof zh;
