import type zh from "../zh/mesh";

export default {
  path: {
    direct: "Direct UDP",
    link: "Control connection",
    udpRelay: "UDP via panel relay",
    tlsRelay: "TLS via panel relay",
    none: "Unreachable",
  },
  pathShort: {
    direct: "Direct",
    relay: "Via panel relay",
  },
  address: "Address",
  udpPort: "UDP port",
  endpoints: "Direct endpoints",
  peer: "Node / panel",
  pathColumn: "Path",
  lastHandshake: "Last handshake",
  lastHandshakeAgo: (ago) => `Last handshake ${ago}`,
  noHandshake: "No handshake yet",
  pingTimeout: "Timed out",
  pingFailed: "Failed",
  rxTx: "Received / sent",
  noPeers: "No connections",
  title: "Connections",
} satisfies typeof zh;
