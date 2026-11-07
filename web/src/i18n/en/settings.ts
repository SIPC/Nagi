import type zh from "../zh/settings";

export default {
  title: "Settings",
  groups: { account: "My account", panel: "Panel", nodes: "Nodes" },
  tabs: {
    profile: { label: "Profile" },
    access: { label: "Panel access", description: "Addresses nodes and browsers use to reach this panel." },
    security: { label: "Account security" },
    members: { label: "Members" },
    alerts: { label: "Alerts" },
    standby: { label: "Standby panels" },
    invite: { label: "Receive nodes" },
    mesh: { label: "Node network" },
    about: { label: "About" },
    update: { label: "Updates" },
  },
} satisfies typeof zh;
