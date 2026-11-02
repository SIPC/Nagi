import type zh from "../zh/access";

export default {
  address: {
    label: "Public address",
    usage: "Nodes and standby panels use it to connect to this panel",
    unset: "Not set",
    edit: "Edit",
    description: "Nodes connect to the panel at public address:panel port. Install commands use the same address.",
    help: "Must be reachable from all nodes. Online nodes receive the new address and use it when they next reconnect. Nodes that are offline during the change must be updated by running nagi panel add on the node.",
  },
  domain: {
    label: "Domain",
    bound: (domain) => `The panel is available at https://${domain}.`,
    unbound: "Leave empty to skip binding and access the panel at https://public-address:port.",
    help: "The domain must already resolve to this machine. Once bound, Caddy on this machine reverse-proxies the panel on ports 80/443 and obtains a certificate automatically. Nodes still connect via the public address and panel port.",
    bind: "Bind",
    unbind: "Unbind",
    edit: "Edit",
    unbindConfirm: (domain) => `Unbind ${domain}? The panel will no longer be available at this domain, and pages opened through it will disconnect.`,
  },
  fingerprint: {
    label: "Public key fingerprint",
    description: "Nodes and standby panels check this fingerprint to confirm they are connecting to this panel.",
  },
} satisfies typeof zh;
