import type zh from "../zh/panels";

export default {
  handover: {
    confirm: (n) =>
      `Hand over ${n === 1 ? "the selected node" : `${n} selected nodes`}? This panel will stop managing them and any projects deployed only on them. The new panel restores these projects from the node cache.`,
    intro:
      "Generate the invite code under Receive nodes in the new panel's Settings. Once the new panel takes over, this panel removes itself from the nodes and stops managing them. Handover works per project: select all related nodes (nodes running its containers, failover nodes, backup nodes and witnesses).",
    code: "Invite code",
    codePlaceholder: "nagi-invite:…",
    byProject: "Select by project",
    byProjectHelp: "Selecting a project also selects the nodes running its containers, its backup nodes, failover nodes and witness nodes.",
    nodes: "Nodes",
    offline: " (offline)",
    processing: "Processing…",
    submit: "Hand over nodes",
    handedOver: "Handed over",
    failed: "Failed",
    finish: "Done",
    codeRequired: "Enter the invite code",
    nodesRequired: "Select the nodes to hand over",
  },
  nodePanels: {
    title: "Trusted panels",
    intro: "The node accepts connections and operations from these panels.",
    empty: "No panels",
    self: "This panel",
    primary: "Primary panel",
    standby: "Standby panel",
    connected: "Connected",
    disconnected: "Not connected",
    publicKey: "Public key",
    remove: "Remove panel",
    lastPanel: "A node must trust at least one panel",
    removeSelfConfirm: "Remove this panel from the node? This panel will stop managing the node. Other panels continue to manage it.",
    removeConfirm: (address, key) =>
      `Remove panel ${address} (public key ${key}…) from the node? That panel will no longer be able to connect to or operate this node.`,
  },
} satisfies typeof zh;
