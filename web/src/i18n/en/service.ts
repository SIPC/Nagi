import type zh from "../zh/service";

export default {
  tabs: { overview: "Overview", logs: "Logs", terminal: "Terminal", metrics: "Monitoring" },
  belongsTo: "In container group",
  notFound: (service, group) => `Container group ${group} has no service ${service}. It may have been deleted.`,
  backToGroup: "Back to container group",
  image: "Image",
  health: "Health check",
  healthLabels: { healthy: "Healthy", unhealthy: "Unhealthy", starting: "Checking" },
  restarts: "Restarts",
  replicas: "Replicas",
  ports: "Port mappings",
  resources: "Resources",
  restartConfirm: (name) => `Restart service ${name}? It will be briefly unavailable. Other services in the container group are not affected.`,
  stopConfirm: (name) => `Stop service ${name}? It will be unavailable. Other services in the container group keep running.`,
} satisfies typeof zh;
