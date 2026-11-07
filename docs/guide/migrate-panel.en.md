# Switch panels

Create an invite code on the new panel and paste it into the old one to hand over nodes. The new panel rebuilds the projects from the nodes' cache; drafts and version history are not kept.

If the old panel is already gone, run on each node:

```bash
nagi panel add <invite-code>
nagi panel remove <old-panel-address>
```
