package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 给 agent 写配置时查的说明，内容摘自 docs/agent.md 和 docs/node-access.md，随程序一起编进去，和当前版本一致。
// 改了那两份文档里相应的规则时，这里要跟着改。
var docs = []struct{ uri, name, title, text string }{
	{"nagi://docs/containers", "containers", "Containers, files and deployment", `# Containers in Nagi

- A project groups containers that work together. Containers in a project reach each other by name on any node:
  a single container by its container name (e.g. db:5432), a container inside a group as <container>.<group> (e.g. db.umami).
  Linked projects reach each other as <container>.<network name>.
- A container is either:
  - an image-form container: image, command, ports, environment variables, persisted paths, memory/CPU limits and a health check.
    Nagi generates its compose file; the service name is the container name.
  - a container group: one compose file; every service is a container of the group. The whole group runs on one node.
- Container names are lowercase letters, digits and -, unique in the project, and cannot be changed later.
- Every change is saved as a draft (update_container) and takes effect on deploy. Each deploy creates a version
  (configuration + files + image digests) that can be rolled back. Data in volumes is not rolled back.
- A deploy is watched for 10 seconds: a container that exits non-zero or restarts in that time fails the deploy,
  and its last log lines are returned.

## Compose rules

- Relative paths are relative to the container's data/ directory (./uploads is <container dir>/data/uploads).
- Named volumes are stored in the container's volumes/ directory and are included in snapshots. external volumes
  and volumes with a custom driver are not.
- Ports: publish with ports only when the port must be reachable on the node. For HTTP, prefer a port mapping
  (save_route) with a domain: Nagi's Caddy serves it with automatic HTTPS.
- Containers on other nodes are reached through proxies for TCP and unicast UDP. Ports not declared with ports/expose
  must be added to the project's internal ports to be reachable from other nodes.

## Variables

- Project shared variables: ${VAR} in every compose file of the project.
- Compose variables of a container group: ${VAR} in that compose file; they override shared ones.
- Environment variables: passed into a container (image form: env; container group: per service).
- Variable values are never returned by the tools; get_container lists their names. Prefer variables over literal
  secrets in compose files.

## What operators cannot save

Operators (not administrators) cannot save compose settings that give root on the node: privileged, cap_add,
devices, security_opt, network_mode/pid/ipc/uts/userns_mode/cgroup set to host, absolute host paths (including
docker.sock), ~ or .. in paths, env_file/secrets/configs outside the container directory, include, extends, build,
volumes with a driver, external volumes and networks, and variables in any of these fields.
`},
}

func addResources(s *mcp.Server) {
	for _, d := range docs {
		text := d.text
		s.AddResource(&mcp.Resource{URI: d.uri, Name: d.name, Title: d.title, MIMEType: "text/markdown"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: d.uri, MIMEType: "text/markdown", Text: text}}}, nil
			})
	}
}
