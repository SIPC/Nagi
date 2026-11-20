# Changelog

Each version's notes are also shown on the panel's "About" page. For version numbering rules, see the [development guide](docs/development.md#发布) (in Chinese).

To upgrade: from v0.6.0, update the panel with one click in its settings; for earlier versions, run the install script again on the panel server. Then upgrade each node with one click on the "Nodes" page.

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh
```

## v0.36.0

### New features

- The "Audit log" permission can be granted to other roles. Members with it can view, export, and verify all audit records.
- TCP and UDP port mappings now report connections, failure rate, active connections, and traffic on the project overview, and are included in the home page rankings.
- The Android app adds a Nodes page with node monitoring, networking, and high availability. Admins can use the node terminal, files, and images.
- The MCP tool query_metrics can query requests, status codes, latency, connections, and traffic of port mappings. Access tokens with only the project "View" permission can query metrics of containers and port mappings.

### Improvements

- Node violations detected by the panel get a summary entry when the combining period ends.
- While in the foreground, the Android app extends the unlock period based on when it actually renews its token, and switching pages no longer restarts the timer.

### Fixes

- Fixed an issue where missing directories in a git repository resulted in incomplete deployments.
- Fixed an issue where a container just redeployed to its original node could be stopped.
- Fixed an issue where the failover holder could fail to renew its lease in some cases and be taken over by the standby.
- Fixed an issue where hiding node names replaced ordinary words in deployment output.
- Fixed an issue where syncing release notes removed the Android app signing certificate digest.
- Fixed an issue where port mapping totals for requests, connections, and traffic were too high, and traffic and failures were undercounted when connections ended.
- Fixed an issue where egress restrictions did not show recently logged targets to non-admins.
- Fixed an issue where the Android app stopped extending the foreground unlock after the system time was adjusted.
- Fixed known vulnerabilities and improved security.

### Behavior changes

- Rolling back, or restoring a deployment, now requires verifying your identity when the target version has privileged configuration that differs from the running configuration. Members without the "Privileged configuration" permission cannot do this.
- When a node runs low on free inodes, snapshots, uploads, replica sync, and git pulls stop.
- Git pulls stop when the repository has more than 1,000,000 files and directories combined, paths deeper than 64 levels, or content under a symbolic link.
- Consecutive spaces in node names are combined into one, unassigned characters are not allowed, and names that look alike are treated as duplicates. The first panel start after the update tidies up old names that do not follow these rules.
- The command for adding a standby panel now passes the pairing code in the environment variable `NAGI_STANDBY`. `--standby` still works. The install script no longer installs versions earlier than v0.35.0 as a standby panel.
- Project settings parameters in Audit and events are shown according to the member's permissions, and file entries are searched only by what the member can see.
- When a member cannot see the standby node, the holder's sync time and failback status are no longer shown.
- Members without the "Audit log" permission no longer see the Audit page. Project and container events are shown as before.

### Upgrade notes

- Upgrade nodes to v0.36.0.

## v0.35.0

### New features

- Custom roles are supported. Permissions decide what each member can view and do, managed in "Settings → Members". Nodes and addresses outside a member's access scope are hidden from non-administrators.
- Identity can be confirmed on a phone. After adding a phone, identity verification and the second step of signing in can be completed in the Android app.
- Upgrading nodes and updating the panel show download progress, speed, and the download address.
- The panel name can be changed in "Settings → Panel access".
- The Android app's home adds a live traffic chart and node status, and its settings can manage your account, members, alerts, standby panels, and more, showing entries according to the member's permissions. Tablets use a two-column layout.
- Adding a phone checks whether its attestation certificate has been revoked by Google. If the certificate of an added phone is revoked, it is marked in the member's signed-in devices in "Members" and an alert is sent.
- MCP adds a deployment guide and warns about discouraged practices when saving and deploying containers.

### Improvements

- The home overview no longer waits for slow nodes, and traffic data appears shortly after.
- The QR code for adding a phone is simpler and larger, and the app scans it faster.
- The Android app shows the last data first when opened, with smoother animations and adding flow.
- Sync status appears immediately after clicking "Sync now" or "Stop sync".
- Repeated failed phone confirmations at sign-in send an alert and are recorded in Audit.

### Fixes

- Fixed an issue where some Xiaomi phones could not be added.
- Fixed an issue where the Android app crashed in some cases.
- Fixed an issue where a stalled download during a panel update was reported as a missing file.
- Fixed known vulnerabilities and improved security.

### Behavior changes

- The preset Operator role no longer includes backup, high availability, migration, network and domains, alert rules, registries, or project management. Edit the role if needed.
- Changing a node's projects root requires the "Node terminal and files" permission and verifying your identity first. Access tokens need "Node terminal" selected.
- Access tokens need "Panel administration" selected to change project groups and node tags.
- Pulling from a git repository is limited to 30 minutes and stops when the node runs low on disk space or the repository has more than 1,000,000 files.
- Ports in compose bound only to a local address (for example `127.0.0.1:5432:5432`) are no longer imported as port mappings.
- Compose files whose ports and expose entries add up to more than 1,000 after expanding ranges can no longer be saved or deployed.
- Snapshots are no longer created when a source directory is a symbolic link.
- Scheduled snapshots and snapshot repository maintenance are limited to 6 hours.
- A node's projects root cannot be set to a system directory such as `/etc`, `/usr`, or `/run`.
- Your own password can no longer be reset in "Members". Change it in "Account security".
- Creating or editing members and roles, generating standby panel pairing codes, handing over nodes, and promoting a standby panel now require verifying your identity, with a password or verification code only.
- Deleting members and roles requires verifying your identity.
- Saving or deploying privileged configuration, opening a terminal in a privileged container, and changing files in a privileged container require verifying your identity.
- Rebuilding a project from a node requires the "Privileged configuration" permission.
- Parameters and failure details in Audit are shown only to members with permission for that action, and webhook URLs only to members with the "Deploy" permission.
- The node install command now passes the join token in an environment variable. If you use sudo, run `sudo -i` first.
- Each invite code can add up to 20 nodes, and up to 20 nodes can be handed over at a time. Nodes with the same name get a number added automatically, and node names cannot contain invisible characters.
- Up to 16 nodes awaiting approval can be connected at a time, and up to 4 from the same source address. The rest connect after approval.
- In MCP, nodes that share a name must be specified by node ID.
- Node join notifications show the source address, and multiple joins with the same invite code in a short time are combined into one notification.
- Terminals opened in the Android app are disconnected after the unlock duration has passed.
- The Android app no longer accepts external pairing links after a panel has been added. The app's content is hidden in recent tasks.
- In the Android app, adding and editing members and adding and promoting standby panels have moved to the web panel.
- The panel no longer accepts avatars larger than 1024 pixels on either side.

### Upgrade notes

- Upgrade nodes to v0.35.0 to fix security issues on nodes. Later node upgrades will also show download progress.
- After updating the panel, update the Android app to v0.35.0.
- Node install commands generated before the update and not yet run must be generated again.
- Check port mappings previously imported from compose in the container's "Network" tab.
- Compose files with more than 1,000 ports must be saved again with fewer ports.
- If untrusted containers have run on a node, we recommend replacing the keys and passwords stored on it.

## v0.34.0

### New features

- The first version of the Android app adds phones by QR code, unlocks the app, and shows the home overview. Add a phone in "Account security".
- Each service in a container group has its own page with overview, logs, terminal, and monitoring, and can be restarted, stopped, or started on its own.
- Container groups in the project bar can be expanded to show each service's status, CPU, and memory.

### Improvements

- Download actions in the file menu are combined into "Download".
- Download commands come in Linux/macOS and Windows versions, and large files are downloaded in parallel segments.
- Backup sync now runs on the source node without a time limit, shows progress, speed, and time remaining, and can be stopped. Interrupted syncs continue from where they left off.
- Tunnel connections between nodes are established faster and transfer data faster.

### Fixes

- Fixed an issue where download tools taking over a download through the panel signed you out.

### Behavior changes

- Download links through the panel only work from the network address they were first used from.
- Service names can no longer start with "-".

### Upgrade notes

- Upgrade nodes to v0.34.0 to control services individually, sync backups in the background, and see sync progress.

## v0.33.0

### New features

- The file menu adds "Copy download link" for use in download tools or on other machines.
- When a node can't use HTTPS for direct download, the file menu offers "Copy download command", which downloads over encrypted HTTP and decrypts and verifies the file locally.
- MCP can get a download link or download command for a file.

### Improvements

- Directories larger than 5 GB are no longer sized automatically. Click "Calculate" to calculate their size.

### Behavior changes

- With direct download on, nodes that can't use HTTPS open an HTTP port (80 by default) when needed for encrypted downloads. You can change the port in the node's settings.

### Upgrade notes

- Upgrade nodes to v0.33.0 to use download links and download commands.

## v0.32.0

### New features

- When a node has a public IP, the browser downloads files directly from the node instead of through the panel.
- Registry mirrors add "Use defaults", and changes take effect immediately without restarting Docker.
- Alert channels add "Allow private addresses" for sending alerts to services on a local network.

### Improvements

- When a panel update or node upgrade fails to download, another download address is tried automatically, and the working address is remembered.
- The file manager shows actual directory sizes, and modification times show the exact date and time.

### Fixes

- Fixed known vulnerabilities and improved security.

### Behavior changes

- Nodes with a public IP serve direct downloads on port 443. You can turn off direct download in the node's settings.
- Alert webhooks can only send to public addresses by default.
- Compose fields that aren't on the allow list, such as `container_name` and `use_api_socket`, can only be saved by an administrator.

### Upgrade notes

- For alert channels that use a local network address, select "Allow private addresses" on the channel.
- Upgrade nodes to v0.32.0 to use direct download and full directory size calculation.

## v0.31.0

### New features

- The node page adds Images, where you can see the images on a node and what uses them, delete unused images, clean up, and pull images (administrators only).
- You can choose where a backup node stores replicas: set a replica directory for the node, or a location for a single container. Existing replicas move to the new location automatically.
- MCP supports the project, node and file management available on the web, including node files, node images and replica locations.
- Access tokens add a "Panel administration" permission for viewing and changing panel settings and alert channels, viewing and exporting audit records, and updating the panel through MCP (administrators only).

### Improvements

- MCP confirmation codes can only be obtained from a preview and expire after a while.
- Changing the panel's public address or domain sends a notification through the alert channels first.
- Audit records no longer include file content written through MCP or alert channel webhook URLs.

### Fixes

- Fixed an issue where a background browser tab or multiple open tabs could falsely report a session key signature mismatch and ask to verify again.
- Fixed known vulnerabilities and improved security.

### Upgrade notes

- Upgrade nodes to v0.31.0 to use image management and replica locations.

## v0.30.0

### New features

- Container volume files and node files can be renamed, moved and copied, and multiple items can be cut, copied and pasted.
- Files can be compressed to zip or tar.gz, and zip, tar.gz and tar archives can be extracted.
- Large files are uploaded in chunks and resume after a network interruption or page refresh, with no 8 GB limit. Uploads show progress, speed and time remaining, and can be paused and canceled.
- Opening a directory calculates the size of its subdirectories automatically.
- When pasting items with the same name, you can choose to replace, keep both or skip. Before replacing volume files, you can create a snapshot first.
- The Projects page adds Reorder, for dragging groups and project cards.

### Improvements

- The sort order and custom order on the Projects page are saved to your account and stay the same across browsers and devices.
- New cross-node connections on a project's internal network are established faster.

### Fixes

- Fixed an issue where deleting volume files, creating directories and restoring files didn't check permissions for privileged containers.
- Fixed an issue where an access token without the "Run commands" permission could still change container files.

### Upgrade notes

- Upgrade nodes to v0.30.0 to use the new file management features.

## v0.29.1

### New features

- The project list can be sorted by "Custom": drag groups, and projects within a group, to reorder them (saved in this browser only).
- A node that repeatedly fails to start after an upgrade goes back to its previous version automatically.
- Terminal replays record typed commands. Input that isn't echoed, such as passwords, is recorded only as a character count. The replay lists the input, and clicking a line jumps to it.
- A terminal that is still open can be replayed from the audit log up to the current point.

### Improvements

- Replays of commands run through MCP include the command and its full output.
- The panel now forwards nodes' audit records, so nodes no longer store credentials for log forwarding destinations.

- Groups animate when they expand and collapse.
- The master key and secure entry moved to Settings > Panel access, and "Security" in Settings is renamed "Security policy".
- Shortening audit record retention, relaxing the egress restriction and turning off a container's own alert rules now ask for confirmation.
- Layout, spacing, type sizes and wording are consistent across pages.

### Fixes

- Fixes an issue where opening a node or container terminal signed you out.
- Fixes an issue where phone browsers were often asked to verify identity again.
- Fixes an issue where switching a phone browser to "desktop site" signed you out.
- Fixes an issue where having several tabs open caused a session key signature mismatch.
- Fixes an issue where the egress restriction blocked containers from reaching domains on their own node.
- Fixes an issue where changing project links or migrating a container reported unauthorized requests between nodes.
- Fixed known vulnerabilities and improved security.

### Behavior changes

- Switching back and forth between Wi-Fi and mobile data no longer asks for identity verification. It's recorded in the audit log.
- Administrators no longer need to verify again when switching between their last 5 verified networks.
- Editing or testing a log forwarding destination and shortening audit record retention require identity verification.
- The egress restriction only allows ports the node exposes on its public addresses.
- You need to sign in again once after upgrading.

### Upgrade notes

- Nodes must be upgraded to v0.29.1 for the egress restriction and unauthorized-request fixes to take effect. Automatic return to the previous version applies to upgrades after v0.29.1.

## v0.29.0

### New features

- Adds an audit log that records panel operations, sign-ins, sensitive reads, MCP tool calls and alerts, with filtering and export.
- Records node terminals, container terminals and command output. Administrators can play them back after verifying their identity.
- Sends the audit log to syslog, HTTP or S3, and can verify that records haven't been altered.
- Adds security alerts for a possibly leaked password, many failed sign-ins, an administrator signing in from a new network, a new administrator, and changes such as editing the secure entry or alert settings.
- Restricts containers from reaching the node itself, the local network and cloud metadata addresses. Set it in the project's Settings > Egress restriction.
- Sends an alert when a node receives an unauthorized request from another node. Administrators can isolate a node with one click.
- Adds alert rules for sustained high node or container CPU, request surges and a high 4xx rate.
- Supports signing in with a passkey (requires a bound domain).
- Restricts the addresses administrators can sign in from, and the addresses an access token can be used from.
- A container can have several backup nodes, and backup nodes can be used together with failover.
- Projects can be put in groups and nodes can have tags. Member access can be set by group and tag.

### Improvements

- Strengthens sign-in sessions: session tokens change regularly, and a session used from another browser, system or script tool is signed out.
- Latency is shown in ms, s or min, and WebSocket connections no longer count toward latency.

### Behavior changes

- "Activity" is renamed "Audit".
- Terminal recording is on by default, and turning it off requires identity verification.
- Egress restriction defaults to "Block" for new projects and "Log only" for projects created before the upgrade.
- Sessions end after 24 hours of inactivity, and last at most 7 days from sign-in.
- Only administrators can save compose files that use a network driver other than bridge, or set network driver options or IP address management.

### Upgrade notes

- Nodes must be upgraded to v0.29.0 for egress restriction, unauthorized-request alerts, multiple backup nodes and the new alert rules to take effect.
- Egress restriction requires nftables on the node.

## v0.28.1

### Fixes

- Fixed known vulnerabilities and improved security.
- Fixes an issue where overlapping syncs of the same container could corrupt the copy on a backup or standby node.
- Fixes an issue where syncing from a newer node to an older node could fail.

### Behavior changes

- Sign-in from one IP is limited after 10 failures, and the count resets an hour after the last failure.
- Failover settings and backup nodes restored from a node's cache now take effect only after an administrator confirms them on the container's "High availability" tab.

### Upgrade notes

- Nodes must be upgraded to v0.28.1 for the sync fixes to take effect.

## v0.28.0

### New features

- Downloads of volume files, files in snapshots and node files can resume, and work with multi-connection download tools.
- Selecting several files uploads them at the same time.

### Improvements

- Syncing to a backup or standby node uses several parallel connections and larger transfer windows, which is faster on high-latency links.
- Links on the HA view are now straight lines.

### Behavior changes

- The panel no longer provides the files v0.1.0 nodes need to upgrade. Run the install script again on such nodes to upgrade them.

### Upgrade notes

- Nodes must be upgraded to v0.28.0 for the transfer improvements to take effect.

## v0.27.1

### Improvements

- The live traffic chart on the overview no longer labels each path. Hover over or tap a path to see its nodes and rate.

### Fixes

- Fixes an issue where syncing a large snapshot repository to a backup or standby node over a slow link failed when the connection dropped midway. The sync now reconnects and continues where it stopped.

## v0.27.0

### New features

- Scheduled snapshots can be turned off. Containers with failover still take a snapshot every 15 minutes.
- Scheduled snapshots can run at fixed times each day. You can set several times, in the browser's time zone when saved.
- A container's "Storage" tab can set its own scheduled snapshots, replacing the project's settings.
- Adds the MCP tool set_snapshot_schedule. update_project can turn snapshots on or off and set daily times.

### Fixes

- Fixes the spacing of request counts of ten thousand or more in Chinese.

### Upgrade notes

- Nodes must be upgraded to v0.27.0 to turn scheduled snapshots off or take them at daily times as set.

## v0.26.0

### New features

- Adds the TCP+UDP protocol to port mappings, which opens TCP and UDP on the same port for services such as game and voice servers.
- TCP and UDP port mappings support "Send client address (PROXY protocol)", so services in the container can get the client's real address.
- The install scripts detect mainland China networks, download through a proxy, and use domestic package and registry mirrors for Docker. Panel updates and node upgrades from the panel later download through the same proxy.

### Upgrade notes

- Nodes used as entry nodes for TCP+UDP port mappings, and nodes involved in mappings that use PROXY protocol, must be upgraded to v0.26.0.

## v0.25.1

### Improvements

- The sign-in page fades in when first opened and slides sideways when moving to and from two-step verification, consistent with the rest of the panel.

### Fixes

- Fixes an issue where password managers couldn't recognize the verification code fields at sign-in and in identity verification.

## v0.25.0

### New features

- When two nodes can't connect directly, traffic is relayed through a node connected directly to both, which is closer than relaying through the panel.
- Adds "Relay for other nodes" to a node's "Settings", which can stop the node from relaying for other nodes.
- The node network probes direct connections from several UDP ports and switches to a working port when one link on the route fails.

### Improvements

- "Nodes → Network" shows which node a connection is relayed through.
- The node network shows the local port used by each direct connection.

### Upgrade notes

- Upgrade nodes to v0.25.0.

## v0.24.2

### New features

- Unconnected links under "Nodes → Network" now show why a direct connection failed, for example when both ends are behind NAT or a firewall blocks the UDP port.

### Upgrade notes

- Upgrade nodes to v0.24.2 to see the reasons.

## v0.24.1

### New features

- Alert rules can be set for each node in the node's "Settings".
- Container alert rules can be set for each project, and a container can have its own rules on the new "Alerts" tab.
- Each alert rule can be turned off.

### Fixes

- Fixes an issue where "About" showed "Checked soon" right after checking for updates.

### Behavior changes

- "Settings → Alerts" now contains only notification channels. Existing alert rules are moved to each node and project after upgrading.

### Upgrade notes

- Upgrade nodes to v0.24.1 for project and container alert rules to take effect.

## v0.24.0

### Improvements

- Nodes accept at most 2 uploads at a time, and stop uploads and replica sync when free disk space runs low.
- Established connections are closed when project link or entry permissions are revoked.
- Open container terminals and logs are closed when an account is removed, demoted, or loses access to the project.

### Fixes

- Fixed known vulnerabilities and improved security.
- Fixes an issue where concurrent MCP writes to the same file could overwrite each other.

### Behavior changes

- For privileged containers deployed by an admin, operators can't open a terminal, run commands, change files, or change the container's variables and file source.
- Only admins can save a compose in which one mount contains another (for example, mounting both . and ./uploads).
- Compose no longer reads the node process's environment variables. Add the values you need as container or shared project variables.
- Git repository URLs can't include a username or token, or point to the node itself. For private repositories, use an SSH URL and a deploy key.
- Deployments are refused when a mount or file reference in the container directory passes through a symbolic link.
- When a node reports that another panel changed a project, the panel no longer rebuilds it from the node automatically. The project is listed on the "Nodes" page for an admin to confirm.

### Upgrade notes

- Upgrade nodes to v0.24.0.
- For containers that use a Git URL with a username or token, switch to an SSH URL and a deploy key before deploying.

## v0.23.1

### Fixes

- Fixes an issue where, after a node restarted, other nodes could drop some of its packets and could no longer recover lost ones, so connections took several seconds to recover.

### Upgrade notes

- Upgrade nodes to v0.23.1.

## v0.23.0

### New features

- Shows release notes in "Settings → About" when a new version is available. Click "More info…" to see the notes for every version between the current and the latest.
- Shows release notes in English when the interface is in English.

### Improvements

- Shows when updates were last checked in "About".
- Centers each column vertically in the node list.

### Behavior changes

- Merges "Updates" into "About". Check for and install panel updates in "About".

## v0.22.2

### Improvements

- Uses the same number format on the overview, node and node network pages, so each node shows the same figures on every page.
- Shows CPU, memory, disk and traffic monitoring in node details, and lists node settings one per row for editing in a dialog.
- Changes the time range for node monitoring and latency history to a drop-down menu.
- Shows statuses in lists as gray text with a colored dot, so only items that need attention appear in color.
- Combines multiple notices in the same place into one that can be expanded.
- Prompts before leaving project settings or the project network with unsaved changes.
- Shows form errors below the related field.
- Keeps only "Restart" and "Redeploy" at the top of the container page, and moves "Upload and deploy" and "Pull and deploy" to the "More actions" menu.
- Stops shrinking text in the topology map in narrow windows, and truncates long names.
- Improves keyboard focus styles and screen reader support.

### Fixes

- Fixes an issue where the topology map overestimated usage on multi-core nodes and showed the outer ring as full.
- Fixes an issue where, after a button was clicked, the loading indicator appeared on a different button.
- Fixes an issue where browsers could offer to save or autofill environment variable secrets.

### Behavior changes

- Shows "Profile" by default when Settings is opened.
- Returns to the previous subpage with the browser's Back button after switching subpages in Settings.

## v0.22.1

### Improvements

- Changes the Nodes page to a list, with each row showing the node's address, CPU, memory, disk and container count, so more nodes fit on one screen.
- Shows the latency chart and P95 for port mappings even when there were no requests in the selected period.
- Shows offline nodes as gray rows in the live traffic on the overview.
- Slides the node terminal horizontally when switching tabs, consistent with other tabs.

### Fixes

- Fixes an issue where viewing the help text in the verification dialog left the page blank when two-factor authentication was not enabled.
- Fixes an issue where the page could not scroll on phones when a swipe started on a chart.
- Fixes an issue where clicking outside the dialog after generating a token closed the dialog, and the token could not be viewed again.
- Fixes an issue where clicking "Cancel" while editing a file discarded the changes without a prompt.

## v0.22.0

### New features

- Adds guides for installing the panel, adding a node, adding a standby panel, upgrading and uninstalling, and switching panels. See [docs/guide](https://github.com/SIPC/Nagi/blob/main/docs/guide/README.en.md).
- Shows a gradient avatar generated from the username for accounts without an uploaded avatar.

### Improvements

- Rewrites panel and node error messages to explain the cause and how to resolve it, and adds support for English.
- Shows the reason on read-only pages, for example that a standby panel can only be viewed, and offers a next step on empty pages.
- Adds field descriptions to the settings, project, container, node and node network pages, and states the effect of each action in confirmation dialogs.
- Validates environment variable names, ports and project settings before saving, and shows errors below the related field.
- Shows request counts of 10,000 or more in abbreviated form.

### Fixes

- Fixed known vulnerabilities and improved security.
- Fixes an issue where an invalid snapshot interval or snapshot limit in project settings was replaced with the default value.
- Fixes an issue where some English text was too long and wrapped.

### Upgrade notes

- The improved node error messages and the fix for alert error messages require nodes to be upgraded to v0.22.0.

## v0.21.0

### New features

- Suggests switching to a standby panel when the primary panel cannot be reached, and lists the standby panel addresses to copy or open. With a domain bound, this notice also appears when a page is opened or refreshed while the primary panel's machine is offline.

### Fixes

- Fixed known vulnerabilities and improved security.

### Behavior changes

- Requires identity verification before enabling two-factor authentication and before allowing OAuth access on the authorization page.
- Limits `network_mode`, `pid`, `ipc` and similar keys in compose files saved by operators to services in the same compose file. Setting `name` on a volume or network is limited to administrators.

### Upgrade notes

- The fix for replica restores requires nodes to be upgraded to v0.21.0.

## v0.20.0

### New features

- Supports canceling node upgrades. Cancel all upgrades on the Nodes page, or cancel a single node in node details. Nodes that are downloading stop the download and stay on their current version.

### Improvements

- Shows speed tests step by step in a dialog, lists QUIC and WireGuard tunnel results separately, and shows progress and recommendations.
- Shows node-to-node traffic relayed through the panel as dashed lines on the overview.

### Fixes

- Fixes an issue where traffic from error-correction parity packets was not counted as node-to-node traffic.
- Fixes an issue where traffic the panel relayed for nodes was counted as external traffic of the panel's node.
- Fixes an issue where traffic between the panel and its own node was subtracted from external traffic, making external traffic appear lower.

### Behavior changes

- Records the reason and continues with the next node when a node fails or times out while downloading the new version during an upgrade of all nodes. The upgrade still stops if a node does not recover after its program is replaced.
- Shortens the timeout for nodes downloading a new version from 15 minutes to 10 minutes, and shows an error when it times out.

### Upgrade notes

- Canceling an upgrade that is downloading requires nodes to be upgraded to v0.20.0. Nodes on earlier versions can only cancel queued upgrades.
- Error-correction traffic statistics and step-by-step speed tests require nodes to be upgraded to v0.20.0. Relayed traffic statistics require the panel to be upgraded to v0.20.0.

## v0.19.0

### New features

- Adds a latency matrix to the "Network" view on the Nodes page, showing the current latency between each pair of nodes. Select a cell to view the latency history in both directions for up to 30 days.
- Adds a speed test to the "Node network" tab of a node, which measures upload and download speeds to other nodes or the panel, compares them with the WireGuard tunnel, and suggests a faster option when one is available. Administrators only.
- Shows packet loss in both directions to each node and the panel in the "Node network" tab of a node.
- Adds a "QUIC fast path" switch to node settings, which can be turned off on networks that restrict UDP.

### Improvements

- Uses QUIC for cross-node container access, traffic forwarding, snapshot sync and file uploads when nodes connect directly, greatly improving speed on links with high latency and packet loss.
- Adds automatic error correction based on packet loss when nodes connect directly, noticeably improving transfer speed on lossy links such as cross-border routes. The "Node network" tab shows the current error-correction strength.
- Enlarges the hover area of the live traffic chart on the overview, so thin lines are easier to highlight.

### Fixes

- Fixes an issue where traffic from downloading and uploading node files through the panel was not counted as node-to-node traffic on the overview.

### Upgrade notes

- Latency history requires nodes to be upgraded to v0.19.0.
- QUIC, automatic error correction, speed tests and packet loss require both ends of a connection to be upgraded to v0.19.0.

## v0.18.1

### Improvements

- Lists images one service per row in the container group overview, and omits image digests and the default Docker Hub prefix. Hover to see the full reference.

### Fixes

- Fixes an issue where a long install command caused a horizontal scroll bar in the dialog when adding a node.

## v0.18.0

### New features

- Adds "Existing directory" when adding a container, which brings an existing compose directory on a node into the panel and leaves the data in place. The .env file in the directory is imported as container group variables, and existing named volumes remain in use. On deployment, the containers that were running are stopped and taken over by the panel, and they are restored automatically if startup fails. Administrators only.
- Allows an existing directory to include subdirectories, such as a database directory, in snapshots and restores them along with the snapshot.

### Fixes

- Fixes an issue where compose files using `env_file` could not be saved in the panel.

### Upgrade notes

- Existing directories require nodes to be upgraded to v0.18.0.

## v0.17.0

### New features

- Adds "Snapshot repositories → Clean up now" to node settings, which immediately reclaims data in snapshot repositories that no snapshot uses, including data left by failed snapshots in earlier versions. Snapshots are checked for integrity after cleanup. Administrators only.

### Upgrade notes

- Cleaning up snapshot repositories requires nodes to be upgraded to v0.17.0.

## v0.16.3

### Fixes

- Fixes an issue where scheduled snapshots were retried every minute after failing. Retry intervals now increase gradually up to 60 minutes and persist across node restarts.
- Fixes an issue where retrying a failed snapshot wrote the same data again, causing disk usage to keep growing. Data already written by a failed snapshot is now reused by the next snapshot.
- Fixes an issue where snapshot repositories were no longer maintained while scheduled snapshots kept failing. Maintenance is now scheduled separately.
- Fixes an issue where stateful containers stayed paused for more than 30 seconds after a snapshot timed out, and reports an error when a container fails to resume.

### Improvements

- Skips scheduled snapshots and sends an alert when free disk space is below 2 GiB or 5%.

### Upgrade notes

- These fixes require nodes to be upgraded to v0.16.3.

## v0.16.2

### Improvements

- Returns a preview before each `node_exec` run and runs the command only after the user confirms. The command and arguments cannot be changed after confirmation.
- Requires a reason for each `node_exec` call, and MCP guides agents to prefer other tools.

## v0.16.1

### New features

- Adds the "Node terminal" option to the OAuth authorization page, so clients such as ChatGPT and Claude on the web can also use `node_exec`. Administrators only.

## v0.16.0

### New features

- Adds MCP support for deleting files in projects, containers and volumes, returning to a previous deployment, and restoring data during a rollback.
- Adds MCP support for setting backup nodes, restoring from a backup node, moving containers, redeploying to another node, and setting up, removing and confirming failover.
- Adds `node_exec` to MCP, which runs commands on a node as root. Administrators only. It requires selecting "Node terminal" when generating an access token, and every run is logged and triggers an alert.
- Returns a preview for all of the irreversible operations above and runs them only after confirmation.

### Upgrade notes

- `node_exec` requires nodes to be upgraded to v0.16.0.

## v0.15.0

### New features

- Adds MCP support for changing project settings, shared variables and container display names, and for linking and unlinking projects.
- Supports all container options available in the web interface in the MCP `update_container` and `add_container` tools, including persistent directories, working directory, health checks, git sources, preserved paths, stateful or stateless mode, deployment dependencies, service-level environment variables and secret variables.
- Adds MCP tools for deployment history, image update checks, private registry login, deleting port mappings, reissuing certificates, syncing to the backup node and viewing backup snapshots.
- Adds MCP support for browsing files in snapshots and restoring a single file or directory from a snapshot.

### Fixes

- Fixes an issue where the draft appeared empty after shared variables were saved, and shared variables were cleared after deployment.

### Behavior changes

- Changes the default scheduled snapshot interval from 60 minutes to 720 minutes, including for projects without an interval set. The frequency of syncing to the backup node changes accordingly.
- Sets the default snapshot limit to 10 and no longer supports unlimited snapshots. Projects without a limit keep only the latest 10 snapshots from the next snapshot onward.

### Upgrade notes

- The new defaults require nodes to be upgraded to v0.15.0. To keep more snapshots, raise the snapshot limit in project settings before upgrading nodes.

## v0.14.1

### New features

- Supports deleting snapshots. Scheduled, manual and pre-deployment snapshots can be deleted in the "Storage" tab of a container. Version files are used for rollbacks and cannot be deleted.
- Adds a snapshot limit. Once it is set in project settings, the oldest snapshots are deleted automatically when a container's scheduled, manual and pre-deployment snapshots together exceed the limit.
- Adds `delete_snapshot` to MCP, which deletes a snapshot after confirmation.

### Upgrade notes

- Deleting snapshots and the snapshot limit require nodes to be upgraded to v0.14.1.

## v0.14.0

### New features

- Adds MCP support for restoring snapshots. A preview is returned before the restore, which runs after confirmation and first creates a protective snapshot of the current data.
- Adds MCP support for reading and writing text files in container volumes, which requires the "Run commands" permission. Overwriting an existing file requires the SHA-256 returned when the file was read.
- Adds MCP troubleshooting tools: `check_routes` checks domain resolution and certificates, `query_events` queries project logs, and `inspect_node_storage` shows node disk usage (administrators only).
- Supports waiting for container health checks to pass in `deploy`.

### Improvements

- Moves logs to a separate page above "Settings" in the sidebar, visible only to administrators. Moves alert settings to "Settings → Panel".
- Allows MCP to specify a revision number when changing a container, and rejects the change if someone else has modified the container.
- Logs rejected MCP calls.

### Behavior changes

- Stops deploying by default in `add_container`.

### Upgrade notes

- `inspect_node_storage` requires nodes to be upgraded to v0.14.0.
- Workflows that rely on `add_container` to deploy automatically need to call `deploy` instead.

## v0.13.3

### Fixes

- Fixes an issue where connected MCP clients could not get the tool list after the panel was updated or restarted, because their existing sessions were no longer valid (ChatGPT reported that the app could not be updated). The MCP service is now stateless, so panel restarts no longer affect connected clients.

### Improvements

- Improves ChatGPT compatibility. OAuth authorization callbacks include the `iss` parameter (RFC 9207), so ChatGPT can use its registered fixed callback URL, and each tool declares the OAuth scopes it requires (`securitySchemes`).
- Records the status code, client address and requested host name in the panel log when an MCP request is rejected, making connection issues easier to troubleshoot.

### Upgrade notes

- Only the panel needs to be updated; nodes do not need to be upgraded. If a connector previously added in ChatGPT or Claude still cannot connect, remove it and add it again.

## v0.13.2

### Fixes

- Fixes an issue where, with a domain bound to the panel, MCP clients completed authorization but could not get the tool list ("action discovery failed"). Requests forwarded by the local Caddy were mistakenly treated as DNS rebinding and rejected.

### Improvements

- Runs tests and the builds for each architecture in parallel in the release process, and reuses the build cache of the main branch, shortening release time.

### Upgrade notes

- Only the panel needs to be updated. The panel's built-in node is updated with the panel, and other nodes do not need to be upgraded separately.

## v0.13.1

### Fixes

- Fixes an issue where, with a domain bound to the panel, the OAuth metadata, authorization page and token endpoint used the panel's local address (`127.0.0.1:<panel port>`), so browsers opened the wrong authorization page and Claude connectors could not connect. The domain the client uses is now used throughout.
- Fixes an issue where, with a domain bound to the panel, the last-used address of access tokens and the rate limit for OAuth dynamic registration treated all requests as coming from the local machine. Both now use the real client address.

### Upgrade notes

- Only the panel needs to be updated. The panel's built-in node is updated with the panel, and other nodes do not need to be upgraded separately.

## v0.13.0

### New features

- Adds MCP access. The panel provides an MCP service at `/mcp`, so agents such as Claude Code, Claude connectors and ChatGPT can view and manage the cluster directly.
- Provides 17 MCP tools: the overview; queries for projects, containers, nodes, monitoring and logs; creating projects, adding containers, changing configuration, deploying, starting and stopping, rollbacks, snapshots and port mappings; and running commands in containers.
- Limits MCP permissions to the role and access scope of the authorizing member. Tokens can separately allow changes and running commands, and both are off by default. Tools that are not authorized do not appear in the tool list.
- Does not return variable values to agents. All operations are recorded in Activity with the source token, along with executed commands and their exit codes.
- Adds access tokens. Generate personal access tokens in "Settings → Account security → Access tokens". Identity verification is required before generating a token, and each token is shown only once. Administrators can view and revoke a member's tokens from the member's ⋯ menu, and resetting a member's password also revokes their tokens.
- Adds OAuth authorization. The panel includes an OAuth 2.1 authorization server with dynamic registration, client metadata documents, PKCE and refresh token rotation for clients that support only OAuth. Members complete authorization after confirming the expiration and permissions on the authorization page.

### Improvements

- Uses solid overlays with shadows for depth in dialogs, menus, drop-downs and tooltips, and the backdrop no longer blurs the background. Dialogs with buttons no longer show a close button and focus the first input when opened. Destructive buttons have higher contrast in dark mode.
- Truncates long content in lists instead of widening the area that contains it.

### Upgrade notes

- The panel migrates its database when updated. If you use a standby panel, it must also be upgraded to v0.13.0 to continue syncing. Nodes do not need to be upgraded.
- Cloud clients reach the panel over the internet, so the panel needs a bound domain with a trusted certificate. With secure entry enabled, the MCP URL must include the entry path, for example `https://<panel address>/<entry>/mcp`.

## v0.12.0

### New features

- Adds access scopes. Operators and viewers can be limited to specific projects and nodes, either when adding a member in "Settings → Members" or from the member's "Edit access", by choosing "All / Selected" for projects and nodes separately. Administrators always have access to everything, and existing members default to all, so they are not affected.
- Hides projects outside a member's scope from lists, the overview and search, and opening their address directly shows "Project not found". Terminals, uploads and volume files are restricted in the same way.
- Shows status and usage on the project page as usual when a project's containers are on nodes outside the member's scope, with only the node name shown. These containers can be moved to a node within the scope.
- Allows only nodes within the scope to be selected when adding or moving containers or setting up failover and backups. The Nodes page shows only containers of projects within the scope.
- Counts only nodes, projects and port mappings within the scope on the overview, and draws node-to-node traffic only between nodes within the scope.
- Adds projects created by members with a limited project scope to their scope automatically. Linked projects outside the scope appear as "Another project" and cannot be unlinked.

### Improvements

- Redesigns the Members page. Each row shows the avatar, display name, username, role and access scope, and two-factor authentication is marked only when it is enabled. Role and access scope are both changed from "Edit access" in the ⋯ menu.
- Expands and collapses content smoothly together with the content below it when switching options such as "All / Selected" or "Form / Compose", instead of jumping first. Dialog height also transitions smoothly when its content changes.
- Prevents long node names from overlapping the port input in the project network.

### Upgrade notes

- The panel migrates its database when updated. If you use a standby panel, it must also be upgraded to v0.12.0 to continue syncing.
- Nodes do not need to be upgraded.

## v0.11.5

### New features

- Adds container display names. Rename a container from the ⋯ menu on the container page. The name is for display only; the container name stays the same, and other containers in the project still reach it by container name. Display names are restored when switching panels.

### Improvements

- Uses the path inside the container, such as `/data`, as the title for volumes of a single container, and hides unused application file directories.
- Shows only rings and values for container resource usage in the project column, with the metric name on hover.

### Upgrade notes

- Available after the panel is updated. Display names are saved to the node cache for restoring when switching panels only after nodes are upgraded to v0.11.5.

## v0.11.4

### New features

- Adds content transitions. After a click on navigation, tabs or segmented controls, content slides in from the direction of the click while the previous content fades out in the same direction. The selection indicator and navigation highlight now move with the leading edge first and the trailing edge following.
- Adds rolling numbers. For values that change, such as monitoring figures, usage, rates, counts and uptime, only the digits that change roll up or down, and the content that follows shifts smoothly when the number of digits changes.

### Improvements

- Transitions each component smoothly to its new height when data finishes loading or list items are added or removed, and moves the content below along with it. Loading placeholders are shorter.
- Keeps the live traffic chart in place when paths are added or removed or the view changes. New paths grow from their row, removed paths retract into their row, and the rest move smoothly. Paths that stop carrying traffic show 0 B/s and remain for 30 seconds before collapsing, so small flows no longer keep changing the layout.
- Keeps the traffic trend legend on one line, collapses items that do not fit into +N, and aligns the left and right charts.
- Shows units for both used and total memory and disk in the node table.
- Makes the hover state of navigation items one step lighter than the selected state, so the two are easy to tell apart.
- Adds enter and exit animations to menus, drop-downs, dialogs and bottom sheets on phones. Overlays and the sticky header on phones use a translucent material, and switching themes cross-fades the whole page. The system "Reduce motion" and "Reduce transparency" settings are respected.

### Upgrade notes

- Front-end changes only. Available after the panel is updated; nodes do not need to be upgraded.

## v0.11.3

### Improvements

- Shows CPU, memory and disk on node cards as three stacked items, each with a full-width usage bar below the text. Bars are neutral by default, turn amber from 75% and red from 90%, and the used value changes color with them.
- Uses the page background for node cards and project cards with only a border, and project cards no longer turn gray on hover.

### Upgrade notes

- Front-end changes only. Available after the panel is updated; nodes do not need to be upgraded.

## v0.11.2

### Improvements

- Uses MiSans Normal for body text, MiSans Regular for emphasis, and MiSans Medium for page, section and dialog titles.
- Enlarges and bolds the project name in the project column, and shows "Project" and the status in small gray text for a clearer hierarchy.
- Keeps the content area at a maximum width of 1120 and centers it on wide screens.
- Narrows the login form to 320 and removes the hint for forgotten passwords. Recovery methods are unchanged; see the [documentation](docs/panel.md#登录) (in Chinese).

### Upgrade notes

- Front-end changes only. Available after the panel is updated; nodes do not need to be upgraded.

## v0.11.1

### Improvements

- Restores "current / limit" for CPU on node cards, for example `12% / 1200%`, with each core counted as 100%. The topology map and node details still show the share of the whole machine.

### Upgrade notes

- Front-end changes only. Available after the panel is updated; nodes do not need to be upgraded.

## v0.11.0

### New features

- Adds a node terminal. The new "Terminal" tab on the node page connects to the node as root without an SSH account or open ports, even when the node is behind NAT. On phones, Esc, Tab, Ctrl and arrow keys are provided.
- Adds node files. The new "Files" tab on the node page supports browsing, editing, uploading, downloading and deleting. Saving checks whether the file has been modified, and files up to 1 MB keep the last 10 versions, which can be restored with one click. The Nagi data directory and system virtual file systems cannot be accessed, container directories are read-only, and sensitive files require confirmation before saving.
- Adds identity verification. Node access is available only to administrators and requires identity verification, which is valid for 15 minutes. Opening and closing terminals and file operations are recorded in Activity, and an alert is sent when a terminal is opened. Terminals disconnect within 30 seconds after signing out or a permission change.
- Adds an About page that shows the version and acknowledgements.

### Improvements

- Records opening a container terminal in Activity.
- Tightens interface spacing, sizes table columns to their content, and uses gray for all secondary text. Body text now uses MiSans Regular and emphasis uses Medium.
- Places the panel on the ring in the node topology map and moves labels outside the ring, so lines no longer cross text.

### Fixes

- Fixes an issue where long image names in the version list overlapped the time.
- Fixes an issue where the traffic chart cut off the last label.
- Fixes an issue where loading skeletons widened the page on phones.

### Security

- Prevents operators from saving privileged configurations, including `privileged`, `cap_add`, `pid: host`, and mounts of `docker.sock` or paths outside the container directory. Existing configurations are not affected, and operators can continue to deploy and roll back.
- Prevents operators from setting a container directory outside the project root directory.

### Upgrade notes

- Node terminal and files require both the panel and the node to be upgraded to v0.11.0. Nodes that have not been upgraded show "Node version too old".
- Node access is enabled by default. To disable it, run `nagi node access disable` on the node; it cannot be enabled again from the panel. For the panel's built-in node, add `--data-dir <panel data directory>/agent`.
- The panel database has a new migration, which runs automatically during the upgrade.

## v0.10.1

### Fixes

- Records node-to-node traffic on nodes. Previously the overview accumulated this data only in the browser, and it was lost on refresh. Nodes now store it locally like other monitoring data, keeping 10-second resolution for 24 hours and 5-minute resolution for 30 days, so history appears as soon as the overview opens and changing the time range takes effect.
- Subtracts node-to-node traffic from external traffic trends at each point in time as well, so both are measured consistently.

### Upgrade notes

- Nodes start recording node-to-node traffic only after being upgraded to this version, so there is no history for the period before the upgrade. The overview indicates nodes that have not been upgraded, whose external traffic still includes node-to-node traffic for now.
- Nodes store two additional monitoring series for each mesh member.

## v0.10.0

### New features

- Adds the overview as the new home page. It shows at a glance the status of nodes and containers, live external and node-to-node traffic, traffic trends, usage per node, and the port mappings with the most traffic. Traffic is estimated from node reports, and node-to-node trends accumulate from when the page is opened.
- Redesigns every page to a new design system, with the MiSans font and consistent lists, forms, notices and dialogs, adapted for light mode, dark mode and phones.
- Adds full-screen navigation from the top bar on phones, dedicated top bars for the file and editor pages, and bottom sheets for the More menu and confirmation dialogs.

### Improvements

- Requires only a name to create a project. After creation, the project overview opens, where you can add the first container.
- Adds a separate page for adding containers, which can be submitted with only an image. The name defaults to the image name, and the node defaults to the online node with the most containers in the project. Choose "Add and deploy" or "Add only".
- Opens file management in place in the Storage tab instead of in a dialog, and the browser's Back button returns to the volume list.
- Shows high availability status first, with settings in a dialog. When action is needed, a notice and a primary button appear at the top of the page.
- Compares the current and target versions in the rollback confirmation, and moves "Also restore data" into the confirmation dialog.
- Simplifies the container page header. Node, directory and current version move to the overview, and image updates move to the More menu.
- Shows skeletons while loading, and always shows a notice with a retry option when a query fails. When alert configuration fails to load, an empty form is no longer shown, so the configuration cannot be overwritten.
- Requires confirmation before discarding a shared variable draft, revoking an invite code, disabling secure entry, unbinding a domain or deleting an alert channel. Alert channels are now saved immediately.

### Upgrade notes

- Front-end changes only, with no changes to data or APIs. Available after the panel is updated; nodes do not need to be upgraded.
- The panel now bundles the MiSans font, so the installation package is slightly larger. The interface font is provided by Xiaomi.

## v0.9.0

### New features

- Adds a user card at the bottom of the navigation that shows the avatar, name and role, and opens the profile, account security, theme, language and sign out.
- Allows each member to upload an avatar and change their display name. Sign-in and Activity still use the username.
- Adds upgrading all nodes with one click. On the Nodes page, outdated nodes are upgraded one at a time to the panel's version. Nodes used only for hot standby and backups are upgraded first, and the upgrade stops if a node fails.
- Adds update notices. Administrators see new versions, update progress and failure reasons in the lower-right corner of any page.

### Improvements

- Stores panel and node update progress on the panel. After refreshing the page or switching browsers, an update in progress no longer appears as available to start again.
- Allows containers and port mappings to be opened with the keyboard, and shows a clear indicator on the current tab and help tooltips when focused. Container and mapping names can be Ctrl-clicked to open in a new tab.
- Shows the reason when hovering over a disabled button in read-only mode. Viewers can no longer reach the create page through "New project".
- Requires confirmation before changing a member's role. After a form submission fails, focus moves to the error message; after a failed sign-in, focus stays in the input.
- Keeps project filters, sorting and the Activity category in the URL, so they persist after refreshing. Shows a notice when copying fails.
- Applies the selected theme to native controls such as scroll bars when it differs from the system theme. File directories show 200 items at a time, with the option to load more.

### Upgrade notes

- The database adds display name and avatar fields to accounts. With a standby panel, both panels must be upgraded to the same version to sync.

## v0.8.1

### Improvements

- Moves request and traffic statistics for port mappings back to the project overview, so all mappings can be seen on one page. The container's "Network" tab no longer shows them.

### Upgrade notes

- Only the panel needs to be upgraded; nodes do not.

## v0.8.0

### New features

- Adds members and roles. Add accounts in "Settings → Members" with the Administrator, Operator or Viewer role. Operators can manage projects and containers, can only view nodes, and cannot see panel settings. Viewers can only view, cannot see configuration or secrets, and cannot open terminals. Rules are enforced by the panel, all existing accounts are administrators, and at least one administrator must remain. Administrators can reset a member's password and clear two-factor authentication.
- Adds signed-in devices. View the devices signed in to the current account in "Account security", and sign out other devices.
- Adds an activity log. View who did what and when, along with alerts sent, in "Settings → Activity". The latest 5,000 entries are kept, and passwords, tokens and environment variables are not recorded.
- Adds global search. Press Ctrl/⌘ + K to jump to projects, containers, nodes and settings. The project list can be filtered and sorted.
- Adds log tools to search, show only the error stream, show timestamps, pause and download.
- Adds draft comparison and save and deploy. The configuration page shows the changes in the draft relative to the current version, and you can deploy right after saving.
- Adds an environment variable editor with one variable per line. Values whose names look like secrets are hidden by default, and you can switch to text for bulk editing.
- Adds a "Containers" tab to the node page, which is available even when the node is offline.
- Allows updating an image with one click from the "New image version" notice on the project page.
- Adds the option to follow the system theme. The theme can be light, dark or system.
- Regroups the container page. Tabs are now "Overview", "Logs", "Terminal", "Config", "Versions", "Network", "Storage" and "High availability". The former "Data" tab is split into Storage and High availability, and old addresses redirect automatically.
- Moves port mappings to the container page. Manage mappings and view request monitoring and internal access addresses in the container's "Network" tab. The project page keeps an overview of all mappings; click a row to go to the container.
- Adds container events. The overview shows the container's recent operations and alerts, and is visible to all roles.
- Adds the Storage tab. View volume sizes, mount paths and free disk space on the node, and add or remove persistent directories. Browse, download, upload and delete files in the interface, and edit small text files. View files in snapshots and restore a single one of them. File contents are transferred over the mesh.

### Improvements

- Shows operation results as toasts. Container names, network names and environment variables are validated as you type, and required fields are marked.
- Reminds you before leaving a form with unsaved changes, and requires confirmation before discarding a draft. Reverting a configuration to match the current version no longer leaves a draft.
- Shows the current page in the browser tab. Addresses that do not exist show a dedicated page, and projects that do not exist are no longer retried.
- Shows a notice at the top of the project list when the standby panel is not connected.
- Shows CPU as a percentage of the whole machine everywhere. Internal terms such as Mesh, Caddy and lease are replaced with clearer wording.
- Uses the project network name as the default directory for new containers. Existing containers are not affected.
- Shows the scheduled interval in the snapshot section and explains which records must be rolled back from versions. Alert rules are grouped by category, the save button on the configuration page stays fixed at the bottom, and the three-column layout no longer repeats the project name and title.
- Shows an explanation when the panel cannot be reached or an API does not exist, instead of only `HTTP 404`.

### Fixes

- Fixes an issue where project totals dropped suddenly because only some nodes had reported in the last interval. When a node is offline, totals now note that they exclude that node.

### Upgrade notes

- The database adds an activity log table and a role field for accounts. All existing accounts become administrators.
- Recording node alerts in Activity requires nodes to be upgraded as well. Nodes that have not been upgraded still send alerts, but the alerts are not recorded.
- The Storage tab requires nodes to be upgraded. Nodes that have not been upgraded prompt for an upgrade.

## v0.7.0

### New features

- Adds manually starting, stopping and restarting containers from the container page, without recreating the container or creating a new version. Container groups are operated on as a whole. Containers with failover set must have failover removed before they can be stopped; restarting is not affected.

### Improvements

- Replaces all browser pop-ups with consistent confirmation dialogs whose primary button names the specific action. Redeploying, deploying a draft, updating an image and batch deploying deployed containers now require confirmation.
- Makes deleting the directory an option, unchecked by default, when deleting a container; when it is checked, the container name must be entered to confirm. Previously, clicking "Cancel" in the confirmation meant deleting only the container, which was easy to do by mistake.
- Uses shadcn/ui for dialogs, menus, drop-down selects, checkboxes, tabs and help tooltips.

### Fixes

- Fixes an issue where projects and containers showed "Partially running" or "Not deployed" while a node was offline. They now show which node is offline and that the container status is unknown. The navigation and project list also indicate offline nodes, which are now shown in red.
- Fixes an issue where container details showed the health status from Docker's text, which could contradict the status column (for example, a green "Running" alongside `unhealthy` after a snapshot pause).
- Fixes an issue where starting a deployment from the "Data" tab did not show progress or results. It now returns to the overview.
- Fixes an issue where monitoring chart tooltips wrapped near the right edge.

### Upgrade notes

- Starting, stopping and restarting require both the panel and the container's node to be upgraded. Nodes that have not been upgraded prompt for an upgrade.

## v0.6.1

### Improvements

- Simplifies domain binding. Once a domain is entered, the port mapping form no longer shows the node port, and access goes through ports 80/443. TCP/UDP mappings do not show domains.
- Adds domain checks. Port mappings show the status of each domain on each entry node: whether it resolves to that node, whether the certificate has been issued and when it expires, and why issuance failed. Resolution results are also shown live when editing a mapping. These checks are informational only and do not block saving; when using a CDN, notices about mismatched resolution can be ignored.
- Adds manual certificate retries. When certificate issuance fails, click "Retry now" instead of waiting for the automatic retry. Each node can retry at most once per minute, and Let's Encrypt allows only 5 failed validations per domain per hour, so fix DNS or the firewall before retrying.

### Fixes

- Fixes an issue where a domain could be bound while ports 80 or 443 on the node were used by another program, such as a separately installed Caddy or Nginx, which caused the node's entire Caddy configuration to fail to load and all mappings on that node to stop working. Binding a domain now checks these ports and refuses to save with a notice.

### Upgrade notes

- Certificate status and manual retries require the panel and entry nodes to be upgraded to v0.6.1. Nodes that have not been upgraded show the certificate status as unknown.
- Mappings that already have a domain bound are not affected. Ports 80 and 443 are checked only when the port or domain is changed, not when containers are deployed.

## v0.6.0

### New features

- Adds one-click panel updates. View the current and latest versions in "Settings → Updates", and the sidebar shows a notice when a new version is available. After you click "Update", the panel downloads the new version, replaces the program only after it passes verification and a trial run, and then restarts in place. The page refreshes automatically, without signing in again. If any step fails, the running panel is not affected. Project containers keep running, and the panel's built-in node is updated with the panel. Standby panels are updated separately on their own pages.

### Improvements

- Shows the version to be installed in the panel install script before downloading, for example "Current version v0.5.1, updating to v0.6.0". When a mirror is specified with `--download-url`, the version is known only after downloading and is shown when installation finishes.

### Upgrade notes

- One-click updates are available from v0.6.0, so this update still requires running the install script again on the panel server. Nodes do not need to be upgraded for this.
- For panels that cannot reach GitHub and were installed with `--download-url`, pass `--download-url` once more when upgrading to v0.6.0. The script saves this address, and later one-click updates download from it. Checking for new versions still requires access to GitHub; if it cannot be reached, continue upgrading with the install script.
- Operations in progress, such as deployments, are interrupted during an update. After updating, you cannot go back to an older version from the panel.

## v0.5.1

### Improvements

- Adds "Update image" to the ⋯ menu on the container page, which pulls the latest image and redeploys at any time without waiting for the daily check to find a new version, including for containers that have never deployed successfully. When a new version is found, the button still appears next to the container page title. If there is an undeployed draft, confirmation is required first, because the draft takes effect too.

## v0.5.0

### New features

- Adds cross-node entry. Port mappings can select multiple "Entry nodes", so when a container runs on node A, external requests can also reach it through the address of node B, and Nagi forwards them to the container over the encrypted channel between nodes. Supports HTTP, TCP and UDP. Multiple entries can be enabled at once, and the container's own node can be deselected so that traffic enters only through other nodes. Entries keep working while the panel is unavailable, and after a container fails over, traffic is forwarded to the new node automatically. Entry nodes can reach only the port the mapping points to, not other containers or ports in the project.

### Improvements

- Lists all entry nodes in the access addresses of a port mapping. Request monitoring for a mapping combines all entry nodes: request counts and traffic are added together, and latency uses the slowest entry.
- Checks the mapping's port on all entry nodes when saving, and refuses to save if it conflicts with another mapping or is used by another program on the node.

### Upgrade notes

- Cross-node entry requires the panel, entry nodes, the container's node and failover nodes to be upgraded to v0.5.0. The interface prompts for an upgrade when any of these nodes has not been upgraded. Existing mappings are not affected and remain open only on the container's node.
- Cross-node traffic is slower than a direct connection, is counted on both the entry node and the container's node, and is relayed through the panel when the two nodes cannot connect directly. Entry nodes must open the ports in their own firewalls.
- TCP/UDP services forwarded through entry nodes cannot see the client's real IP address; HTTP services can still get it from request headers.
- When a domain is bound, each entry node tries to obtain a certificate, so select only the nodes the domain actually resolves to. When multiple entries are needed, terminate HTTPS at a CDN and enable "HTTP only".
- Entry nodes themselves are not highly available. If one fails, traffic is not moved automatically, and DNS or clients must switch to another entry.

## v0.4.2

### Improvements

- Counts all local disks on the node (block devices and ZFS) for disk capacity on node cards and the details page. Cards show the combined "used / total", and hovering shows each disk. Multiple mount points of the same disk are counted once. Loop devices, read-only images, network storage, and mounts under boot partitions, system directories and container runtime directories are excluded, but the disks holding the node directory and project directories are always included.

### Upgrade notes

- Showing the total of all disks requires the panel and nodes to be upgraded to v0.4.2. Nodes that have not been upgraded still show the disk holding the data directory.
- Disk alerts are unchanged and still apply only to the disks holding the node directory and project directories. The disk chart on the details page is also unchanged and still shows the disk holding the node directory.

## v0.4.1

### Fixes

- Fixes an issue in the node topology map where, with only two nodes, the line and latency between them covered the panel and its name. The two nodes are now arranged in a triangle with the panel, and with more nodes, lines passing near the panel also route around it.
- Fixes an issue where lines in the topology map passed through the latency and container count labels. The labels now have a background.

## v0.4.0

### New features

- Adds a node topology map. The new "Network" view on the Nodes page places the panel in the center with nodes in a ring, and lines show how nodes connect: directly, relayed through the panel or unreachable. Hover over a node to see latency; connections that are not direct are listed below the map.
- Adds a high availability view. Arrows show the hot standby and backup nodes for containers on each node, and stateful containers without replicas are marked. Select a node to simulate its failure and list the outcome for its containers, such as automatic failover, failover after confirmation, restorable from backup or data unrecoverable.
- Adds node renaming. Change a node's name in the panel from the ⋯ menu on the node details page without affecting its host name. Alerts from the node also use the new name.
- Adds registry mirrors. Configure Docker registry mirror addresses in the node's "Settings". The addresses are written to `/etc/docker/daemon.json` and take effect after Docker restarts, with a notice until then.

### Improvements

- Unifies spacing, cards, forms and tables across the interface, switches to the system font for better mixed Chinese and English text, and tightens page margins on narrow screens. Project cards list up to 5 containers and show the rest as "N more".
- Shows the replicas, trusted panels and Mesh tabs on the node details page as tables.
- Shows hot standby and witness nodes in failover settings, and the source nodes of replicas, by each node's current name.

### Upgrade notes

- Using the panel's node names in node alerts requires nodes to be upgraded to v0.4.0. Nodes that have not been upgraded still show the host name in alerts.

## v0.3.0

### New features

- Adds cross-node UDP forwarding in the project network. When containers in a project access each other across nodes, unicast UDP is forwarded in addition to TCP, for example for DNS, game servers and QUIC. UDP ports declared by containers are included automatically; add undeclared ports in "Network → Internal ports" with `/udp` at the end, for example `dns:53/udp`. Project links also support UDP.
- Adds node upgrade notices. When a node's version is older than the panel's, the node list shows "Upgrade available" and the node details page offers an upgrade button. Only official releases are compared, and downgrades are never suggested.

### Fixes

- Fixes an issue where cross-node access to a node that had restarted could be interrupted for more than ten seconds, or until the next report. It now recovers within about 1 second.
- Fixes an issue where other nodes immediately removed a node from the mesh when it briefly left the member list during a restart.

### Upgrade notes

- Cross-node UDP forwarding requires the nodes involved to be upgraded to v0.3.0. Nodes that have not been upgraded continue to forward TCP.

## v0.2.0

### New features

- Adds node hardware information. Nodes report CPU cores, total memory and disk capacity, the node list shows the hardware, and usage bars now show "used / total".
- Adds a new logo. The interface and images use the wave logo that matches the website icon.

### Improvements

- Downloads the same version of nagi as the panel from GitHub Release for node installation and one-click upgrades, and verifies it against SHA256SUMS. If GitHub cannot be reached, add `--download-url <mirror address>` to the end of the install command, and later one-click upgrades use the same address. The panel no longer needs to provide binaries for each architecture.
- Reduces the binary size by about 8 MB by compiling only the Caddy modules in use.
- Extends the vertical axis of the CPU chart to at least 1%, so small fluctuations in idle containers are no longer magnified into spikes. CPU usage near 0 is shown as `<0.1%`.

### Upgrade notes

- Nodes on v0.1.0 can be upgraded with one click from a v0.2.0 panel. The panel downloads the binary for its own version from GitHub Release, verifies and caches it, and serves it to the older nodes.

## v0.1.0

First public release.

- Organizes containers into projects. A project consists of multiple containers, each running on a node of your choice and reaching the others by name across nodes. Compose is the only underlying definition and can be exported at any time.
- Saves changes as drafts before deployment, and records a version for each deployment, which can be rolled back, or the whole project can be restored to its state at a given deployment. Includes logs, a terminal and image update reminders.
- Includes Caddy for public access, with domains and automatic HTTPS, and TCP/UDP port forwarding.
- Provides high availability with snapshots and backup nodes, container moves, and per-container failover nodes with automatic failback. Primary and standby panels keep services running when the panel fails, and a new panel can rebuild projects from node caches.
- Provides monitoring for nodes, containers and port mappings, with alerts via webhook and Telegram.
- Offers the interface in Chinese and English, with dark and light themes. Wide screens use a three-column layout of navigation, projects and containers.
- Provides a one-click install script for the panel, which installs Docker automatically and registers the `nagi` command. Nodes join by copying a command from the panel. A one-click uninstall script is also provided.
