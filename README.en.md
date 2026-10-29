<p align="center"><img src="docs/assets/hero.png" alt="Nagi · calm waters" width="860"></p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="License: Apache-2.0"></a>
  <img src="https://img.shields.io/badge/platform-Linux%20amd64%20%7C%20arm64-lightgrey" alt="Platform: Linux amd64 | arm64">
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" alt="Go 1.26">
  <a href="https://github.com/SIPC/Nagi/releases/latest"><img src="https://img.shields.io/github/v/release/SIPC/Nagi" alt="Release"></a>
</p>

<p align="center"><a href="README.md">简体中文</a> · <b>English</b></p>

# Nagi (凪)

> Calm waters.

Nagi is an open-source Docker panel that aims to be simple, good-looking and easy to deploy. It is ready for production out of the box and supports multiple nodes and high availability.

The panel UI is available in English and Chinese. The figures below and the [design docs](docs/README.md) are in Chinese.

## Features

![One panel, many nodes](docs/assets/architecture.png)

![Reach containers by name across nodes](docs/assets/intranet.png)

![Automatic failover when a node goes down](docs/assets/failover.png)

![Services keep running when the panel goes down](docs/assets/recovery.png)

![A project is a group of containers](docs/assets/model.png)

![One entry point with automatic HTTPS](docs/assets/exposure.png)

![Every deployment is a version](docs/assets/versions.png)

Also included:

- Logs, terminal, monitoring and alerts (webhook, Telegram); alerts are still sent while the panel is offline
- Snapshots: scheduled per-container backup and restore, with an optional backup node as an off-site replica; move containers between nodes without losing data; redeploy containers to another node if a node is lost for good
- Environment variables at three levels (project, container group, container), secrets, private registry credentials, health checks and resource limits
- Image update notices and registry mirrors for nodes in mainland China
- Projects are isolated by default and can be linked to each other on demand
- Primary and standby panels, and handing nodes over between panels
- Panel security: optional two-factor authentication, login rate limiting and a secure entry path; one-click node upgrades from the panel

Not supported yet: automatic scheduling, multiple replicas of one container, several panels managing the same node, multiple users, restore drills, adopting existing projects, building images on nodes, and app templates. Cross-node traffic between containers is limited to TCP and unicast UDP.

## Quick start

### Install the panel

Run as root on a Linux server:

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh
```

The script downloads nagi, verifies its SHA-256 and runs the panel as a systemd service. The panel includes an embedded node, so a single server is enough to get started, and Docker is installed automatically if it is missing. When it finishes, it prints the panel address, the public key fingerprint and a setup link; open the link in a browser to create the admin account. The panel uses a self-signed certificate: when the browser warns about it, check the fingerprint and continue.

Common options (append to the command, for example `| sudo sh -s -- --port 28443`):

| Option | Description |
| --- | --- |
| `--port <port>` | Panel port; picked at random on first start by default |
| `--no-agent` | Run only the panel without an embedded node, e.g. on a dedicated machine |
| `--docker-mirror <name>` | Package mirror for installing Docker: `Aliyun` or `AzureChinaCloud`, for servers in mainland China |
| `--download-url <url>` | Download nagi from a mirror or internal address when GitHub is unreachable |
| `--version <version>` | Install a specific version; the latest by default |

### Add a node

Copy the install command from the **Nodes** page in the panel and run it as root on the new server. The script downloads the same nagi version as the panel from GitHub Releases, verifies its SHA-256 and installs Docker if needed. If GitHub is unreachable, append `--download-url <mirror>`; one-click upgrades from the panel will use the same address later.

![Join a node with one command](docs/assets/join.png)

### Add a standby panel

Create a pairing code in **Settings → Standby panels** on the primary panel, then run on the new server:

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh -s -- --standby <pairing-code>
```

If the primary panel fails, promote the standby panel to primary.

### Upgrade

Run the install script again to upgrade the panel; its port, data directory and other settings stay the same. Nodes can be upgraded with one click on the **Nodes** page.

### Uninstall

The same script works for panels and nodes:

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/uninstall.sh | sudo sh
```

By default only the program and services are removed; project containers and the Nagi data directory are kept. Add `-s -- --purge` to also remove the containers, networks and data directory managed by Nagi. Project files (container directories, under `/srv/nagi` by default) are never deleted; the script lists where they are. Docker itself is not uninstalled. Before removing a node, move or delete its containers in the panel.

### Switch panels

Create an invite code on the new panel and paste it into the old one to hand over nodes; the new panel rebuilds the projects from the nodes' cache (drafts and version history are not kept). If the old panel is already gone, run on each node:

```bash
nagi panel add <invite-code>
nagi panel remove <old-panel-address>
```

## Requirements

- Linux (amd64 / arm64); systemd recommended
- Docker with the compose plugin 2.24 or later (installed by the install script)
- Ports to open:

| Port | Protocol | Purpose |
| --- | --- | --- |
| Panel port | TCP, UDP | Web console and node connections; must be reachable from browsers and all nodes. UDP is used for WireGuard relaying |
| 80, 443 | TCP | Project domains and certificate issuance; only on nodes that serve projects by domain |
| Node WireGuard port | UDP | Direct node-to-node traffic, picked at random on first start; relayed through the panel if blocked |

Ports you open for containers in the UI must also be allowed on the corresponding nodes.

## Architecture

There is a single binary, `nagi`: `nagi server` runs the panel and `nagi agent` runs a node.

- **Control plane**: nodes connect out to the panel over mTLS, so they need no inbound ports. One connection is multiplexed with yamux, and the panel calls nodes over reverse gRPC. The panel has its own CA, and nodes verify it by public key fingerprint.
- **Data plane**: nodes form a full mesh with userspace wireguard-go, with addresses derived from public keys. When a direct path is unavailable, traffic falls back to UDP relaying and then TLS relaying through the panel.
- **Authorization**: being on the network grants nothing by itself; every call is authorized by the caller's public key.

See the [design docs](docs/README.md) (in Chinese) for details on each module.

## Development

- Backend: Go, gRPC, Connect-RPC, SQLite (modernc)
- Frontend: Vite, React, TypeScript, TanStack Router / Query, shadcn/ui

Building from source and local development are covered in the [development guide](docs/development.md#本地开发) (in Chinese).

## Security

Please do not report security issues in public issues; see [SECURITY.md](SECURITY.md) for how to report them.

## Acknowledgements

Nagi is built on these open-source projects:

- [Caddy](https://github.com/caddyserver/caddy) and [caddy-l4](https://github.com/mholt/caddy-l4): external access, automatic HTTPS and TCP / UDP forwarding
- [wireguard-go](https://git.zx2c4.com/wireguard-go): the encrypted network between nodes
- [kopia](https://github.com/kopia/kopia): snapshots and off-site replicas
- [yamux](https://github.com/hashicorp/yamux) and [Connect](https://connectrpc.com): control-plane communication
- [shadcn/ui](https://ui.shadcn.com): UI components

## License

[Apache-2.0](LICENSE)
