import type zh from "../zh/invite";

export default {
  description: (addNode) => <>Used when another panel hands over nodes. To add a new machine, use {addNode}.</>,
  addNode: "Add node",
  help: "Invite codes are valid for 1 hour and can be used by multiple nodes. Nodes that join with an invite code must be approved on the Nodes page before use, in case the code leaks.",
  generate: "Generate invite code",
  pasteHint: (command) => `Paste this invite code when handing over nodes on the original panel, or run ${command} on the node.`,
  command: "nagi panel add <invite-code>",
  revoke: "Revoke",
  revokeAction: "Revoke invite code",
  revokeConfirm: (id) => `Revoke invite code ${id}? It can no longer be used to add nodes.`,
  usedCount: (n) => (n === 1 ? "Used once" : `Used ${n} times`),
  expiresIn: (time) => `Expires ${time}`,
  empty: "No active invite codes",
} satisfies typeof zh;
