# Add a standby panel

Create a pairing code in **Settings → Standby panels** on the primary panel, then run on the new server:

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh -s -- --standby <pairing-code>
```

The standby panel keeps an up-to-date copy of the primary and is read-only. If the primary panel fails, promote the standby panel to primary.
