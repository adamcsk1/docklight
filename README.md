# Docklight

Docklight is a small Docker Compose stack monitor and manager. It runs in one container, uses the Docker socket, scans a mounted compose root, and stores only stable preferences in SQLite.

## Purpose

Docklight is built for a family NAS or home lab where services are managed as simple Docker Compose stacks. It is intentionally not a full Docker management platform like Portainer and does not try to cover every Docker feature or every deployment style.

The goal is to keep common stack-based workflows straightforward: see what is running, start and stop stacks, pull updates, recreate containers when needed, and keep a small activity trail. If you need broad Docker administration, cluster management, complex role-based access, or deep container editing, Docklight is probably not the right tool.

## Features

- Scan compose folders under `STACKS_ROOT` on startup and on demand.
- Detect `docker-compose.yml`, `docker-compose.yaml`, `compose.yml`, and `compose.yaml`.
- Show live Docker Compose container state.
- Start, stop, restart, rebuild, upgrade, and down compose stacks.
- Store stack preferences, sessions, and activity logs in SQLite.
- Create and edit stack-local managed folders, files, and stack safety options tracked in `.docklight.config.yml`.
- Configurable activity retention, with a 30 day default.
- Optional verbose troubleshooting logs in the data folder.
- One admin user from environment variables.
- Configurable base URL, including `/docklight` behind a reverse proxy.
- Plain dark responsive UI with server-rendered HTML and small vanilla JavaScript.
- Linux and Windows Docker Desktop compatible when paths are mounted into the container.

## Security

Mounting `/var/run/docker.sock` gives Docklight powerful control over the Docker host. Do not expose it publicly without a trusted reverse proxy and HTTPS.

The app refuses to start unless these are set:

- `ADMIN_USER`
- `ADMIN_PASSWORD`
- `SESSION_SECRET`

## Quick Start

Create a compose file like this:

```yaml
services:
  docklight:
    image: ghcr.io/adamcsk1/docklight:latest
    container_name: docklight
    ports:
      - "7070:8080"
    environment:
      BASE_URL: /
      ADMIN_USER: admin
      ADMIN_PASSWORD: change-this-password
      SESSION_SECRET: replace-with-a-long-random-secret
      STACKS_ROOT: /stacks
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./stacks:/stacks
      - ./data:/data
    restart: unless-stopped
```

Open `http://localhost:7070/`.

For deployment options, image tags, Windows paths, private registries, reverse proxies, and self-update setup, see [Docker Deployment](docs/docker.md).

## Environment

| Variable | Default | Description |
| --- | --- | --- |
| `ADDR` | `:8080` | Listen address inside the container. |
| `BASE_URL` | `/` | URL prefix. Use `/docklight` behind a path-based reverse proxy. |
| `ADMIN_USER` | required | Single admin username. |
| `ADMIN_PASSWORD` | required | Single admin password. |
| `SESSION_SECRET` | required | Long random secret for sessions. |
| `STACKS_ROOT` | `/stacks` | Mounted compose root inside the Docklight container. |
| `STACKS_HOST_ROOT` | `STACKS_ROOT` | Docker-daemon-visible host path for the same stack root. Set this when stack compose files use relative bind mounts and `/stacks` is not the same path on the Docker host. |
| `DB_PATH` | `/data/docklight.db` | SQLite database path. |
| `COOKIE_SECURE` | `false` | Set to `true` when serving only through HTTPS. |

See [Docker Deployment](docs/docker.md) for Linux and Windows volume examples, `STACKS_HOST_ROOT`, reverse proxy paths, private registry credentials, and self-update requirements.

## Stored Data

SQLite stores durable app data only:

- discovered stack relative path
- compose filename
- sessions
- activity log
- app settings, including activity retention and verbose logging state

Activity retention defaults to 30 days and can be changed on the Config page. The Activity page can download all retained activity as CSV.

Verbose logging is off by default. When enabled on the Config page, Docklight writes timestamped troubleshooting logs to the data folder.

When Docklight creates or edits extra stack-local folders, files, or stack safety checkboxes, it stores those stack-specific settings in `.docklight.config.yml` inside that stack folder. Only files listed there are editable in Docklight; manual files in old or hand-created stacks are left untouched unless added through the stack edit UI.

It does not store container IDs or live Docker state. If the user changes containers with the Docker CLI, Docklight reads the live state again.

## More Documentation

- [Docker Deployment](docs/docker.md) covers image tags, path mapping, private registries, reverse proxies, stored data, and upgrade behavior.
- [Development](docs/development.md) covers maintainer tasks, CI publishing, vendored frontend dependencies, and Go dependency updates.

## Current Limitations

- Stack `Down` runs `docker compose down` only. Stack deletion removes compose-created volumes after explicit confirmation; standalone unused volume deletion is available from the Volumes page.
