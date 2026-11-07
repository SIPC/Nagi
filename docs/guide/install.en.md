# Install the panel

Before you start, make sure the server meets the [requirements](../../README.en.md#requirements).

Run as root on a Linux server:

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh
```

The script downloads nagi, verifies its SHA-256 and runs the panel as a systemd service. The panel includes an embedded node, so a single server is enough to get started, and Docker is installed automatically if it is missing. When it finishes, it prints the panel address, the public key fingerprint and a setup link; open the link in a browser to create the admin account. The panel uses a self-signed certificate: when the browser warns about it, check the fingerprint and continue.

## Common options

Append to the command, for example `| sudo sh -s -- --port 28443`:

| Option | Description |
| --- | --- |
| `--port <port>` | Panel port; picked at random on first start by default |
| `--no-agent` | Run only the panel without an embedded node, e.g. on a dedicated machine |
| `--docker-mirror <name>` | Package mirror for installing Docker: `Aliyun` or `AzureChinaCloud`, for servers in mainland China |
| `--download-url <url>` | Download nagi from a mirror or internal address when GitHub is unreachable; one-click panel updates then use it too |
| `--version <version>` | Install a specific version; the latest by default |

Next: [Add a node](add-node.en.md).
