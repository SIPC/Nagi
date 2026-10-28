import type zh from "../zh/containerForm";

export default {
  image: "Image",
  command: "Command",
  commandHint: "Leave empty to use the image's default command",
  ports: "Published ports",
  portsHint: "Format: node port:container port, separated by spaces",
  portsHelp:
    "For example 8080:80 53:53/udp. Converted to port mappings on first deployment and exposed via Caddy. Afterwards, edit them or bind domains on the Network tab.",
  workdir: "Working directory",
  workdirHelp:
    "Mount path inside the container for the container's data/ directory (uploaded files or files pulled from Git). Leave empty to skip mounting.",
  volumes: "Persistent directories",
  volumesHint: "Paths inside the container, one per line",
  volumesHelp: "Data is stored under volumes/ in the container directory and included in snapshots, backups and migrations.",
  env: "Environment variables",
  envHint: "KEY=VALUE, one per line",
  advanced: "Resource limits and health check",
  memory: "Memory limit (MB)",
  noLimitHint: "Leave empty for no limit",
  cpus: "CPU limit (cores)",
  cpusHint: "Decimals allowed, e.g. 0.5. Leave empty for no limit",
  health: "Health check command",
  healthHelp: "Runs inside the container via sh -c. Exit code 0 means healthy; 3 consecutive failures mark it unhealthy.",
  healthInterval: "Check interval (seconds)",
  healthIntervalHint: "Defaults to 30 seconds",
  type: "Type",
  typeImage: "Container",
  typeCompose: "Container group (compose)",
  composeHelp:
    "The whole compose file becomes one container group, suited to apps that bundle dependencies such as a database. Containers in the group reach each other by compose service name.",
  dependsOn: "Dependencies",
  dependsOnHint: "Container names, comma-separated",
  dependsOnHelp: "In batch deployments, the containers listed here are deployed first, e.g. the database a backend depends on.",
} satisfies typeof zh;
