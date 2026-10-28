import type zh from "../zh/invite";

export default {
  title: "Invite codes",
  description: "Used when handing over nodes from another panel. To add a new machine, use Add node on the Nodes page.",
  help: "Invite codes are valid for 1 hour and can be used by multiple nodes. Nodes that join with an invite code must be approved on the Nodes page before use, in case the code leaks.",
  generate: "Generate invite code",
  pasteHint: (command) => <>Paste this invite code when handing over nodes on the original panel, or run {command} on the node.</>,
  command: "nagi panel add <invite-code>",
  id: "Invite code ID",
  uses: "Uses",
  expires: "Expires",
  usesCount: (n) => (n === 1 ? "1 use" : `${n} uses`),
  revoke: "Revoke",
  empty: "No active invite codes",
} satisfies typeof zh;
