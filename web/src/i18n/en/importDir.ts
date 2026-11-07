import type zh from "../zh/importDir";

export default {
  description: "Bring an existing Compose directory on a node under the panel. Data stays in place, and deploying takes over the containers running there.",
  dir: "Directory",
  dirHelp: "Absolute path on the node of the directory with the Compose file.",
  read: "Read directory",
  elevate: "Reading a directory on the node requires verifying your identity, valid for 15 minutes.",
  composeFile: "Compose file",
  variables: "Imported variables",
  variablesHelp: "The directory's .env is copied into container group variables.",
  none: "None",
  replaces: "Replaces",
  replacesValue: (project, containers) => `${project}: ${containers}`,
  nothingRunning: "No containers are running in this directory",
  replaceNotice: "Deploying stops these containers and deletes them once the new ones are up (data and volumes are kept). If the new ones fail to start, the old ones are restored.",
  limits: "The data is outside the container directory, so snapshots don't include it. Migration, failover and backup nodes aren't available.",
  limitsWithBackup: "The data is outside the container directory, so snapshots include only the subdirectories entered above. Back up the rest separately. Migration, failover and backup nodes aren't available.",
  importAndDeploy: "Import and deploy",
  importOnly: "Import only",
} satisfies typeof zh;
