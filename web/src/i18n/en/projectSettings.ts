import type zh from "../zh/projectSettings";

export default {
  general: {
    title: "General",
    name: "Project name",
    networkName: "Network name",
    networkNameHint: "Lowercase letters, digits and -. Unique on the panel",
    networkNameHelp:
      "Linked projects reach this project's containers at \"container.network-name\". After changing it, update the addresses those projects use. Existing container directories don't change.",
    snapshotInterval: "Snapshot interval (minutes)",
    snapshotIntervalHint: "Defaults to 60 minutes if empty",
  },
  sharedEnv: {
    title: "Shared variables",
    draft: "Draft",
    description: "Reference them as ${VAR} in any container's compose.",
    help: "Container variables take precedence over shared ones with the same name. Changes are saved as a draft and apply on deploy.",
    vars: "Variables",
    varsHint: "KEY=VALUE, one per line",
    scope: "Deploy to",
    scopeHelp: "Containers that reference changed variables are selected by default.",
    discardDraft: "Discard draft",
    saveDraft: "Save draft",
    deploy: "Deploy",
  },
  deployments: {
    title: "Deployment history",
    description: "Each deployment records every container's version. Restore the project to any past deployment.",
    empty: "No deployments yet",
    columns: {
      id: "ID",
      time: "Time",
      source: "Source",
      versions: "Versions",
    },
    sources: {
      manual: "Manual deploy",
      git: "Git pull",
      image: "Image update",
      rollback: "Container rollback",
      sharedEnv: "Shared variables",
      restore: "Restore",
    },
    containerList: (names) => names.join(", "),
    failed: "Failed",
    restore: "Restore",
    restoreConfirm: (id) => `Restore the project to deployment #${id}? Containers on a different version will be rolled back.`,
  },
  delete: {
    title: "Delete project",
    description: "Stop and delete all containers and container groups in the project.",
    help: "Container directories, including data and snapshots, are kept on their nodes by default. The containers' nodes must be online.",
    confirm: (name) => `Delete project ${name}? All containers and container groups in the project will be deleted. Container directories are kept.`,
    deleteDir: "Also delete container directories (data and snapshots can't be recovered)",
    confirmName: (name) => `Type ${name} to confirm`,
    submit: "Delete project",
  },
} satisfies typeof zh;
