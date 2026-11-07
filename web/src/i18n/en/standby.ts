import type zh from "../zh/standby";

export default {
  promote: {
    confirm:
      "Promote this panel to primary?\n\n" +
      "· Don't promote while the primary panel is still running, or the two panels may make conflicting changes.\n" +
      "· After promotion, nodes only accept changes from this panel, and other standby panels sync from this panel instead.\n" +
      "· When the old primary panel recovers, it automatically becomes a standby panel. Changes from its last few seconds that weren't synced are lost.",
    done: "Promoted to primary panel. The panel is restarting and this page will reload automatically.",
    button: "Promote to primary",
  },
  connected: "Connected",
  disconnected: "Not connected",
  lastSync: "Last synced",
  status: {
    title: "Sync status",
    help: "This is a standby panel that syncs data from the primary panel in real time.",
    primary: "Primary panel",
    connection: "Connection",
    neverSynced: "Not synced yet",
    syncFailed: "Sync failed",
  },
  list: {
    title: "Paired standby panels",
    help: "Standby panels connect to this panel through the panel port and share its CA and master key. Standby panels are read-only; all changes are made on the primary panel.",
    add: "Add standby panel",
    empty: "No standby panels",
  },
  remove: "Remove standby panel",
  removeConfirm: (name) =>
    `Remove standby panel ${name}? It will no longer be able to sync data from this panel. Its data directory must be deleted manually.`,
  disconnectedHint: "The standby panel isn't connected. Check that it's running and reachable.",
  dialog: {
    title: "Add standby panel",
    description: "Run this command on the standby panel's machine.",
    done: "Done",
    notice: "The data directory must be empty. The pairing code is valid for 1 hour and can be used once. Once started, the standby panel connects to this panel, starts syncing and appears in the list.",
  },
  down: {
    title: "This panel is unavailable",
    withStandby: "The panel's machine may be offline. You can continue on a standby panel.",
    noStandby: "The panel's machine may be offline. This notice disappears when it comes back.",
    open: "Open",
    note: "Standby panels are read-only. If the primary panel can't be recovered, promote a standby panel to make changes. Sign in to each panel separately; if secure entry is enabled, add the entry path to the address.",
    retry: "Retry",
  },
} satisfies typeof zh;
