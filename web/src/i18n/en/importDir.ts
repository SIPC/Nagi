import type zh from "../zh/importDir";

export default {
  description: "Bring an existing compose directory on a node under the panel. Data stays where it is, the directory's .env is copied into container group variables, and deploying replaces the containers running there.",
  dir: "Directory",
  dirHelp: "Absolute path on the node of the directory with the compose file.",
  read: "Read",
  elevate: "Reading a directory on the node requires verifying your identity, valid for 15 minutes.",
  composeFile: "Compose file",
  variables: "Imported variables",
  none: "None",
  replaces: "Replaces",
  nothingRunning: "No containers are running in this directory",
  replaceNotice: "Deploying stops these containers and deletes them once the new ones are up (data and volumes are kept). If the new ones fail to start, the old ones are restored.",
  limits: "The data is outside the container directory: snapshots don't include it, and it cannot be migrated, given failover or a backup node.",
  importAndDeploy: "Import and deploy",
} satisfies typeof zh;
