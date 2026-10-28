import type zh from "../zh/alerts";

export default {
  savedAndPushed: "Saved and pushed to online nodes",
  testSent: (name) => `Test message sent to ${name}`,
  channels: "Notification channels",
  lostTitle: "Alert configuration can't be decrypted",
  lostBody:
    "The panel master key was regenerated, so the existing alert configuration can't be decrypted. Re-enter and save it. Until then, nodes keep sending alerts with the old configuration, but the panel can't send its own node offline alerts.",
  addChannel: "Add channel",
  noChannels: "No channels configured. Alerts won't be sent.",
  name: "Name",
  type: "Type",
  target: "Target",
  botToken: "Bot token",
  chatId: "Chat ID",
  webhookUrl: "Webhook URL",
  test: "Test",
  rules: "Trigger rules",
  rulesHelp: "Leave a field empty to use its default value, shown in gray.",
  rule: {
    diskPercent: "Disk usage (%)",
    memPercent: "Memory usage (%)",
    memMinutes: "Memory duration (minutes)",
    restartCount: "Restart count",
    restartMinutes: "Restart window (minutes)",
    certDays: "Certificate days remaining",
    offlineSeconds: "Node offline time (seconds)",
    errorPercent: "5xx error rate (%)",
  },
} satisfies typeof zh;
