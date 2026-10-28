import type zh from "../zh/settings";

export default {
  title: "Settings",
  tabs: {
    access: { label: "Panel access", description: "Addresses nodes and browsers use to reach this panel." },
    security: { label: "Account security", description: "Sign-in password, two-factor authentication and secure entry." },
    alerts: { label: "Alerts", description: "Notification channels and trigger rules for node and container issues." },
    standby: {
      label: "Standby panels",
      description: "Panels that sync this panel's data in real time and can be promoted to primary if the primary panel fails.",
    },
    invite: { label: "Receive handover", description: "Invite codes other panels use to hand over nodes to this panel." },
    mesh: { label: "Mesh", description: "This panel's address in the mesh network and its connections to nodes." },
  },
} satisfies typeof zh;
