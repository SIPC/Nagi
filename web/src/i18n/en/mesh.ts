import type zh from "../zh/mesh";

export default {
  path: {
    direct: "Direct UDP",
    link: "Control connection",
    udpRelay: "UDP via panel relay",
    tlsRelay: "TLS via panel relay",
    none: "Unreachable",
  },
  address: "Address",
  udpPort: "UDP port",
  endpoints: "Direct endpoints",
  peer: "Node / panel",
  pathColumn: "Path",
  lastHandshake: "Last handshake",
  rxTx: "Received / sent",
  noPeers: "No connections",
  title: "Connections",
  help: "Nodes connect to each other and to the panel over direct UDP when possible, and through the panel relay otherwise.",
} satisfies typeof zh;
