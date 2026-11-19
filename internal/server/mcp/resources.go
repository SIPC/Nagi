package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 给 agent 写配置时查的说明，随程序一起编进去，和当前版本一致：
//   - nagi://docs/containers：概念和 compose 的规则，摘自 docs/agent.md 和 docs/node-access.md；
//   - nagi://docs/deploy：怎么部署（单个服务用镜像表单、值放进变量、数据留在容器目录、内部服务不开放端口……），
//     依据 docs/agent.md（创建方式、环境变量、部署方式、项目内网、出站限制）、docs/exposure.md（compose 里写的 ports）
//     和 docs/node-access.md（操作员不能保存的配置）。advice.go 的提醒按它检查，tools.go 的服务说明是它的摘要。
//
// 改了这些文档里相应的规则时，这里要跟着改。
var docs = []struct{ uri, name, title, text string }{
	{"nagi://docs/containers", "containers", "Containers, files and deployment", `# Containers in Nagi

- A project groups containers that work together. Containers in a project reach each other by name on any node:
  a single container by its container name (e.g. db:5432), a container inside a group as <container>.<group> (e.g. db.umami).
  Linked projects (connect_project) reach each other as <container>.<network name>, or
  <service>.<group>.<network name> for a service in a group.
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
- Ports: on the first deploy, ports (image form and compose) are imported as port mappings that Caddy opens to the
  outside on all of the node's addresses; host_ip is ignored, only ports bound to 127.0.0.1 or ::1 are not imported.
  They are imported once: change them later with save_route. Do not publish ports of internal services such as
  databases. For HTTP, use a port mapping (save_route): with a domain, Nagi's Caddy serves it with automatic HTTPS;
  without one, on a published_port as plain HTTP at <node address>:<port>.
- Containers on other nodes are reached through proxies for TCP and unicast UDP. Ports not declared by the image
  (EXPOSE) or with ports/expose must be added to the project's internal_ports to be reachable from other nodes,
  as <container>:<port> or <service>.<group>:<port> (/udp for UDP).

## Variables

- Project shared variables: ${VAR} in every compose file of the project.
- Compose variables of a container group: ${VAR} in that compose file; they override shared ones.
- Environment variables: passed into a container (image form: env; container group: per service).
- Variable values are never returned by the tools; get_container lists their names. Never write secrets as literal
  values in compose files; plain settings may be literal, but variables are preferred (see nagi://docs/deploy).

## What operators cannot save

Operators (not administrators) cannot save compose settings that give root on the node: privileged, cap_add,
devices, security_opt, network_mode/pid/ipc/uts/userns_mode/cgroup set to host, absolute host paths (including
docker.sock), ~ or .. in paths, env_file/secrets/configs outside the container directory, include, extends, build,
volumes with a driver, external volumes and networks, and variables in any of these fields.
`},
	{"nagi://docs/deploy", "deploy", "Deployment guide: do and don't", `# Deploying apps in Nagi

Follow these rules when you add, change or deploy containers. They keep each container's configuration on the
panel and its data in its container directory, so versions, rollback, snapshots, backups, migration and failover
cover all of it. add_container, update_container, import_compose_dir, deploy and a few other tools return warnings
when a configuration breaks them: fix it, or tell the user why it stays.

## 1. Pick the form

| Situation | Use |
| --- | --- |
| One service (an image to run) | image form: add_container with image |
| An app with several services you set up yourself (web, db, cache) | one image-form container per service, in one project |
| The user gives a compose file (e.g. an app's official docker-compose.yml) | container group: add_container with compose |
| Services that must run on the same node in one compose project | container group |

- Do not write a compose file for a single service.
- Separate containers are deployed, rolled back, snapshotted, migrated and failed over one by one: the web container
  can move or roll back while the database stays. A group does all of this as one unit and always runs on one node.
- Services must share a compose project when they share a named volume or a network namespace
  (network_mode: service:<name>), or when the user wants them versioned and rolled back as one unit.

## 2. Projects, names and addresses

- Containers that work together go in one project. Containers reach each other only by name, on any node:
  - a single container: <container> (db:5432);
  - a service in a container group: <service>.<group> (db.umami); inside the group, services use their compose names (db).
  - Never use container IPs (they change) or localhost (inside a container it is the container itself).
- Another project: link the projects with connect_project, then use <container>.<network name>, or
  <service>.<group>.<network name> for a service in a group. Changing a project's network name breaks these addresses
  in linked projects.
- Container names are role names (db, api, web): lowercase letters, digits and -, unique in the project, not the same
  as a service of a container group in the project, and they cannot be changed later.
- Across nodes, containers reach the ports the image or compose declares (EXPOSE, ports, expose). Add others to the
  project's internal_ports (update_project) as <container>:<port> or <service>.<group>:<port>, with /udp for UDP
  (db:5432, db.umami:5432, dns:53/udp). In a container group, declare internal ports with expose, not ports.

## 3. Ports and domains

- Do not publish ports for databases, caches and other internal services (image form: leave ports empty).
  On the first deploy, Nagi imports the ports of the image form and of compose as port mappings, served by Caddy and
  open to the outside on all of the node's addresses: host_ip is ignored. Only ports bound to a loopback address
  (127.0.0.1:5432:5432) are not imported.
- Serve HTTP with save_route and a domain (automatic HTTPS). Without a domain (the user only has an IP), use
  save_route with published_port instead: plain HTTP at <node address>:<port>, no HTTPS. domains does not accept IPs.
  Ports 80 and 443 are reserved for domains, and ports that clash with other mappings on the node are skipped.
- Ports are imported only once: after the first deploy, changing ports in the image form or compose does not change
  the mappings. Change public ports and domains with save_route.
- Before retry_cert, check with check_routes that the domain's DNS points at the node: Let's Encrypt allows only a
  few failed validations per domain per hour.

## 4. Configuration in Nagi variables

- Image form: env sets the container's environment; env_secret marks secrets (passwords, tokens, keys): they are
  hidden in the web UI, and tools never return variable values. Values are passed as written, without ${} substitution,
  so set a value in every container that needs it.
- Container group: write NAME: ${NAME} in the compose file and set the value with env (the group's compose variables,
  env_secret for secrets), or with update_shared_env when several groups of the project use the same value.
  service_env injects variables into one service without a line in the compose file.
- Never hard-code secrets in compose: passwords, tokens, keys, credentials, and passwords inside connection strings
  (postgres://app:<password>@db). Write them as ${NAME} (only the password part of a connection string:
  postgres://app:${DB_PASSWORD}@db:5432/app) and mark them secret with env_secret. Plain settings (NODE_ENV, TZ,
  ports) may be written as values in compose, but prefer variables for anything that differs between deployments.
- Do not write .env files (write_file) and do not use env_file: Nagi generates the container directory's .env itself
  and never passes other .env files to compose.
- Shared variables are saved as a draft: after update_shared_env, deploy with apply_shared_env.
- Do not reach the node or the LAN (host.docker.internal, host-gateway, 172.17.0.1, LAN IPs): new projects block
  connections from containers to the node, the LAN and cloud metadata. Run the database in Nagi too, or ask an
  administrator to allow the address in the project's outbound settings.

## 5. Data

- Image form: volumes lists the persisted paths, paths inside the container to keep (/var/lib/postgresql/data).
  Nagi stores them in the container's volumes/ directory.
- Compose: relative paths (./uploads is <container dir>/data/uploads) or named volumes (stored in volumes/).
- Do not mount host paths outside the container directory: absolute paths (/opt/..., /data/..., /root/...), ~ or ..,
  or a named volume bound to a host directory (driver_opts device). Snapshots, backups, migration and failover do not
  include them, and the path may not exist on another node. Operators cannot save them at all.
- Do not set stateful to no for databases or other containers with data: stateless containers are not paused for
  snapshots, get no predeploy snapshot and are taken over without confirmation.
- An image-form container with workdir runs from data/, which is replaced when files are pulled from git, uploaded or
  rolled back. If the app writes runtime data there, list it in preserved_paths or use volumes.
- Pin a major version for containers with data (postgres:16), not latest or no tag: an update can bring a major
  version that cannot read the existing data.
- With backup nodes or failover, keep scheduled snapshots on (backup nodes sync after each one); use a short
  interval for databases (set_snapshot_schedule).
- Changes made with exec inside a container are lost when it is recreated. Files written with write_file into .data
  are replaced on git pull, upload and rollback, except preserved paths.

## 6. Images and files

- Do not use build: nodes do not build images. Build in CI, push to a registry and use image; log in to private
  registries with login_registry, not docker login on the node.
- With git_url, deploy with pull_git to fetch the files. For a private repository, the user must first add the
  container's deploy key (shown on its page in the web UI) to the repository; tools cannot read it.

## 7. Compose fields Nagi handles

- Do not write container_name (Nagi names containers), network_mode: host (bypasses Caddy, the project network and
  outbound limits), or external networks and volumes (outside the project and the container directory).
- restart and logging are not needed: Nagi adds defaults. Containers with failover are kept running by the nodes and
  cannot be stopped by hand.

## 8. Changing and deploying

- Changes are drafts until deploy. Pass expected_revision (update_container) and expected_revisions (deploy) from
  get_container, so a change made by someone else in between is refused.
- Configure a health check (image form: health_command; compose: healthcheck) and deploy with wait_healthy_seconds.
  Without a health check, a passed result only means the containers are running.
- Read the warnings of every deploy result: ports that were or were not imported, anonymous volumes, a failed
  predeploy snapshot.
- depends_on only orders containers deployed together; after a node restart or across nodes there is no order, so
  apps must retry their connections.
- Resources are not limited by default. With many containers on one node, set memory_mb (image form) or mem_limit.
- Do not run docker run or docker compose up/stop/rm with node_exec, and do not edit compose.yml, override.yml or
  .env in container directories with node file tools: every deploy rewrites them, and these changes bypass drafts,
  versions and failover. Use update_container, deploy and control_container.

## Example: a web app with PostgreSQL

Do: two image-form containers in one project.

    add_container {"project": "shop", "name": "db", "image": "postgres:17",
      "env": {"POSTGRES_USER": "shop", "POSTGRES_DB": "shop", "POSTGRES_PASSWORD": "<generated>"},
      "env_secret": ["POSTGRES_PASSWORD"],
      "volumes": ["/var/lib/postgresql/data"],
      "health_command": "pg_isready -U shop"}

    add_container {"project": "shop", "name": "web", "image": "ghcr.io/acme/shop:1.4.2",
      "env": {"DATABASE_URL": "postgres://shop:<generated>@db:5432/shop"},
      "env_secret": ["DATABASE_URL"],
      "depends_on": ["db"], "health_command": "wget -qO- http://localhost:8080/health"}

    deploy {"project": "shop", "containers": ["db", "web"], "wait_healthy_seconds": 60}
    save_route {"project": "shop", "container": "web", "port": 8080, "domains": ["shop.example.com"]}

No ports are published: web reaches the database as db:5432, and HTTP goes through the port mapping. (localhost in
web's health check is fine: the check runs inside web.)

Don't: one compose file with literal secrets, a host path, an env_file and a published database port.

    services:
      db:
        image: postgres:latest                   # no pinned version for a database
        environment:
          POSTGRES_DB: shop                      # fine: a plain setting
          POSTGRES_PASSWORD: s3cret              # literal secret in the compose file
        ports:
          - "5432:5432"                          # opened to the outside on the first deploy
        volumes:
          - /opt/pg:/var/lib/postgresql/data     # host path: not in snapshots, backups or migrations
      web:
        image: ghcr.io/acme/shop:1.4.2
        env_file: .env                           # a file Nagi does not manage
        environment:
          DATABASE_URL: postgres://shop:s3cret@172.18.0.2:5432/shop   # literal password and a container IP
        ports:
          - "8080:8080"                          # use save_route with a domain

The password is stored in plain text in every version, the database is reachable from the internet, its files stay
behind when the container migrates or fails over, and web cannot be deployed or rolled back without db.

## When a container group is right

The user gives a compose file, e.g. an app's official docker-compose.yml. Keep its structure and its plain settings,
but move secrets to variables, keep data in the container directory and drop ports of internal services:

    services:
      app:
        image: ghcr.io/umami-software/umami:postgresql-v2.18
        environment:
          DATABASE_URL: postgresql://umami:${DB_PASSWORD}@db:5432/umami
          APP_SECRET: ${APP_SECRET}
        depends_on: [db]
      db:
        image: postgres:16
        environment:
          POSTGRES_DB: umami
          POSTGRES_USER: umami
          POSTGRES_PASSWORD: ${DB_PASSWORD}
        volumes:
          - db-data:/var/lib/postgresql/data
        healthcheck:
          test: ["CMD-SHELL", "pg_isready -U $${POSTGRES_USER}"]
    volumes:
      db-data:

    add_container {"project": "analytics", "name": "umami", "compose": "<the file above>",
      "env": {"DB_PASSWORD": "<generated>", "APP_SECRET": "<generated>"},
      "env_secret": ["DB_PASSWORD", "APP_SECRET"]}
    save_route {"project": "analytics", "container": "umami", "service": "app", "port": 3000, "domains": ["stats.example.com"]}

Inside the group the services use their compose names (db); other containers of the project reach them as app.umami
and db.umami. If the user's file has literal secrets, env_file, host paths or published internal ports, convert them
like this, or tell the user what stays as it is and why.
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
