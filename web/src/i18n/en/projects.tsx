import type zh from "../zh/projects";

export default {
  reminders: {
    totpTitle: "Two-factor authentication is off",
    // 链接自带左右外边距（mx-1），当作空格；句末不加句号，免得句号前空一格。
    totpText: (link) => <>Enable it in{link}</>,
    totpLink: "Settings › Account security",
    masterKeyTitle: "Back up the panel master key",
    masterKeyText: (path) => (
      <>Secrets in the panel database are encrypted with the master key and can't be decrypted without it. Back up {path} separately.</>
    ),
    dismiss: "Don't remind me",
  },
  title: "Projects",
  description: "A project is a group of containers that work together. Containers can run on different nodes and reach each other by name.",
  newProject: "New project",
  empty: {
    title: "No projects",
    text: "Create a project, then add containers or container groups (a compose file) and choose a node for each container.",
  },
  card: {
    stateful: "Stateful",
    noContainers: "No containers",
    nodeOffline: "Node offline",
    tookOver: "Failed over",
    failover: "Failover",
    containers: (n) => (n === 1 ? "1 container" : `${n} containers`),
    portMappings: (n) => (n === 1 ? "1 port mapping" : `${n} port mappings`),
    rebuiltAt: (date) => `Rebuilt ${date}`,
    createdAt: (date) => `Created ${date}`,
  },
  nodeChoice: {
    node: "Node",
    nodeHelp: "The container runs on this node. You can migrate it to another node later.",
    noOnlineNodes: "No online nodes",
    dir: "Container directory",
    dirHelp:
      "Holds the container's runtime files, data and snapshots. Must be an absolute path to a directory that doesn't exist or is empty. Defaults to <projects root>/<project name>/<container name>, with Chinese characters converted to pinyin and a suffix such as -2 added on conflict.",
  },
  create: {
    description: "Projects aren't deployed automatically. Deploy manually once the configuration is ready.",
    basics: "General",
    name: "Name",
    namePlaceholder: "My app",
    networkName: "Network name",
    slugHint: "Lowercase letters, digits and -",
    networkHelp:
      "Unique within the panel. Linked projects reach this project's containers at <container>.<network name>. Defaults to a name derived from the project name, with Chinese characters converted to pinyin.",
    firstContainer: "First container",
    firstContainerDescription: "Optional. You can add more containers after creating the project.",
    withContainer: "Create with project",
    groupName: "Container group name",
    containerName: "Container name",
    containerNameHelp:
      "Can't be changed after creation. A single container uses this name on the project network; containers in a container group are named <container>.<group name>.",
    submit: "Create project",
  },
} satisfies typeof zh;
