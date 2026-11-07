import type zh from "../zh/projects";

export default {
  attention: {
    title: (n) => (n === 1 ? "1 item needs attention" : `${n} items need attention`),
    // 跟在标题后面的名单，英文一律小写。
    nodes: (names) => (names.length === 1 ? `node ${names[0]} offline` : `${names.length} nodes offline`),
    standby: "standby panel disconnected",
    totp: "two-factor authentication off",
    masterKey: "master key not backed up",
    view: "View",
    collapse: "Collapse",
  },
  loadFailed: {
    title: "Can't load projects",
  },
  reminders: {
    totpTitle: "Two-factor authentication is off",
    // 链接自带左右外边距（mx-1），当作空格；句末不加句号，免得句号前空一格。
    totpText: (link) => <>Turn it on in{link}</>,
    totpLink: "Account security settings",
    masterKeyTitle: "Master key not backed up",
    masterKeyText: (path) => (
      <>Secrets in the panel database are encrypted with the master key and can't be decrypted without it. Back up {path} separately.</>
    ),
    dismiss: "Don't remind me",
  },
  title: "Projects",
  description: "A group of containers that work together. They can run on different nodes.",
  newShort: "New",
  newProject: "New project",
  empty: {
    title: "No projects",
    text: "Create a project, then add containers or container groups (a compose file) and choose a node for each container.",
  },
  filter: {
    placeholder: "Filter projects or containers…",
    label: "Filter projects or containers",
    sort: "Sort",
    byCreated: "Newest",
    byName: "Name",
    empty: "No matching projects",
  },
  offline: {
    body: "The state of containers on the node is unknown. They may still be running.",
    action: "View nodes",
  },
  standbyOffline: {
    title: (names) => `Standby panel ${names} is disconnected`,
    body: "The standby panel isn't syncing this panel's data and can't take over if the primary fails.",
    action: "View standby panels",
  },
  card: {
    stateful: "Stateful",
    noContainers: "No containers",
    tookOver: "Failed over",
    failover: "Failover set",
    imageUpdate: "Image update available",
    containers: (n) => (n === 1 ? "1 container" : `${n} containers`),
    portMappings: (n) => (n === 1 ? "1 port mapping" : `${n} port mappings`),
    moreContainers: (n) => `${n} more`,
    rebuiltAt: (date) => `Rebuilt ${date}`,
    createdAt: (date) => `Created ${date}`,
  },
  nodeChoice: {
    node: "Node",
    nodeHelp: "The container runs on this node. You can migrate it to another node later.",
    noOnlineNodes: "No online nodes",
    noOnlineNodesHint: "No online nodes. Containers can't be added.",
    viewNodes: "View nodes",
    dir: "Container directory",
    dirHelp:
      "Holds the container's runtime files, data and snapshots. Must be an absolute path to a directory that doesn't exist or is empty. Defaults to <projects root>/<network name>/<container name>, reusing the project's folder on this node if one exists and adding a suffix such as -2 on conflict.",
  },
  create: {
    editNetwork: "Edit",
    name: "Name",
    namePlaceholder: "My app",
    networkName: "Network name",
    slugHint: "Lowercase letters, digits and -",
    networkHelp:
      "Unique within the panel. Linked projects reach this project's containers at <container>.<network name>. Defaults to a name derived from the project name, with Chinese characters converted to pinyin.",
    groupName: "Container group name",
    submit: "Create project",
  },
} satisfies typeof zh;
