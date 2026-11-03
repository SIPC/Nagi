import type zh from "../zh/settings";

export default {
  title: "Settings",
  groups: { account: "My account", panel: "Panel", nodes: "Nodes" },
  tabs: {
    profile: { label: "Profile", description: "Display name and avatar." },
    access: { label: "Panel access", description: "Addresses nodes and browsers use to reach this panel." },
    security: { label: "Account security", description: "Sign-in password, two-factor authentication and secure entry." },
    members: { label: "Members", description: "Accounts that can sign in to this panel, and their roles." },
    alerts: { label: "Alerts", description: "Notification channels and trigger rules for node and container issues." },
    standby: {
      label: "Standby panels",
      description: "Panels that sync this panel's data in real time and can be promoted to primary if the primary panel fails.",
    },
    invite: { label: "Receive nodes", description: "Invite codes other panels use to hand over nodes to this panel." },
    mesh: { label: "Node network", description: "This panel's address in the private node network and its connections to nodes." },
    about: { label: "About", description: "" },
    update: { label: "Updates", description: "See the panel version and update to the latest release." },
  },
} satisfies typeof zh;
