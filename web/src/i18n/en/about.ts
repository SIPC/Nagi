import type zh from "../zh/about";

export default {
  tagline: "A self-hosted panel for deploying and managing containers across nodes.",
  links: {
    title: "Project",
    source: "Source code",
    changelog: "Changelog",
    issues: "Report an issue",
    license: "License",
  },
  credits: {
    title: "Acknowledgements",
    caddy: "Reverse proxy and automatic HTTPS, with caddy-l4",
    wireguard: "Encrypted network between nodes",
    quic: "QUIC fast path and BBR congestion control",
    kopia: "Snapshots and off-site copies",
    control: "Control-plane communication",
    shadcn: "UI components",
    misans: "UI font, by Xiaomi",
  },
} satisfies typeof zh;
