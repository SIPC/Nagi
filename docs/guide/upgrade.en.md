# Upgrade and uninstall

## Upgrade

Update the panel with one click under **Settings → Updates**, or run the install script again; its port, data directory and other settings stay the same. Nodes can be upgraded with one click on the **Nodes** page.

## Uninstall

The same script works for panels and nodes:

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/uninstall.sh | sudo sh
```

By default only the program and services are removed; project containers and the Nagi data directory are kept. Add `-s -- --purge` to also remove the containers, networks and data directory managed by Nagi. Project files (container directories, under `/srv/nagi` by default) are never deleted; the script lists where they are. Docker itself is not uninstalled.

Before removing a node, move or delete its containers in the panel.
