import type zh from "../zh/settings";

export default {
  title: "Settings",
  tabs: {
    access: { label: "Panel access", description: "Addresses nodes and browsers use to reach this panel." },
    security: { label: "Account security", description: "Sign-in password, two-factor authentication and secure entry." },
    alerts: { label: "Alerts", description: "Notification channels and trigger rules for node and container issues." },
    activity: { label: "Activity", description: "Operations on this panel and alerts sent. The latest 5,000 entries are kept." },
    standby: {
      label: "Standby panels",
      description: "Panels that sync this panel's data in real time and can be promoted to primary if the primary panel fails.",
    },
    invite: { label: "Receive nodes", description: "Invite codes other panels use to hand over nodes to this panel." },
    mesh: { label: "Node network", description: "This panel's address in the private node network and its connections to nodes." },
    update: { label: "Updates", description: "See the panel version and update to the latest release." },
  },
} satisfies typeof zh;
