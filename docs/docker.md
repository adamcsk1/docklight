# Docker Deployment

This page covers Docker Compose deployment details for Docklight. For a quick overview, start with the [README](../README.md).

## Compose Example

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

## Published Image

Docklight images are published to GitHub Container Registry:

```text
ghcr.io/adamcsk1/docklight
```

Use the published image instead of a local build:

```yaml
image: ghcr.io/adamcsk1/docklight:latest
```

For stable deployments, pin a version tag instead of `latest`:

```yaml
image: ghcr.io/adamcsk1/docklight:v1.2.3
```

Available tag styles:

- `latest` tracks the default branch.
- `v1.2.3`, `1.2.3`, `1.2`, and `1` are published from Git tags such as `v1.2.3`.
- `sha-<commit>` is published for exact commit pinning.

If the GHCR package is private, authenticate on the Docker host before pulling:

```bash
docker login ghcr.io -u <github-user>
```

Use a GitHub token with `read:packages`. Public packages do not require login.

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
| `STACKS_FILE_OWNER` | unset | Optional Linux-only numeric `uid:gid` applied to stack files Docklight creates, such as `.docklight.config.yml`, compose files, managed files, and managed folders. Use this to keep host bind-mounted stack files editable by your host user while Docklight still runs as root for Docker socket access. |
| `DATA_FILE_OWNER` | unset | Optional Linux-only numeric `uid:gid` applied to Docklight-created data artifacts, including the data directory, `config.yml`, SQLite database files, and verbose logs. It does not recursively change all files under `/data` or manage Docker config ownership. |
| `DB_PATH` | `/data/docklight.db` | SQLite database path. |
| `COOKIE_SECURE` | `false` | Set to `true` when serving only through HTTPS. |

## Linux Volume Paths

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
  - /home/administrator/docker:/stacks
  - ./data:/data
environment:
  STACKS_ROOT: /stacks
  STACKS_HOST_ROOT: /home/administrator/docker
  STACKS_FILE_OWNER: "1000:1000"
  DATA_FILE_OWNER: "1000:1000"
```

`STACKS_HOST_ROOT` must point to the same host folder as the `/stacks` mount. Docklight keeps the Compose project directory inside the container, but rewrites stack-relative bind mounts such as `./data:/app/data` to the Docker-daemon-visible host path for start, upgrade, recreate, and delete actions. Compose project names are pinned to the Docklight stack name.

Set `STACKS_FILE_OWNER` to the numeric `uid:gid` of the host user that should edit stack files. Docklight applies it to stack-local files and folders it creates, including `.docklight.config.yml`, without changing how the Docklight container itself runs. Running the whole container with Compose `user: "1000:1000"` can also avoid root-owned files, but it often requires extra Docker socket, `/data`, and Docker config permissions.

Set `DATA_FILE_OWNER` when Docklight-created files under `/data`, such as `config.yml`, `docklight.db`, and verbose logs, should be editable from the host without root. Docklight applies it only to its own data directory and files; it does not recursively chown `/data` or change Docker CLI credential mount ownership. If you mount Docker CLI credentials, create and chown that credential directory on the host.

## Windows Docker Desktop Paths

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
  - ${USERPROFILE}/docker:/stacks
  - ./data:/data
environment:
  STACKS_ROOT: /stacks
  STACKS_HOST_ROOT: /host_mnt/c/Users/YourUser/docker
```

For Windows Docker Desktop, use the Docker daemon path for `STACKS_HOST_ROOT`. A path under `C:\Users\YourUser\docker` is usually visible to the daemon as `/host_mnt/c/Users/YourUser/docker`.

Docklight stores container-internal paths like `/stacks/immich` as the source of truth. `STACKS_HOST_ROOT` is only used at command execution time to preserve relative bind mount resolution.

Create the Windows folder before starting the container. If Docker reports `mkdir C:\Users\Adam: Access is denied`, the host path is wrong for your machine, the folder does not exist, or Docker Desktop does not have access to that drive/folder. Prefer `${USERPROFILE}/docker` or an absolute path that exists under your current Windows user.

## Private Registry Access

Docklight's image pulls run the Docker CLI inside the Docklight container. The mounted Docker socket gives access to the host Docker daemon, but it does not automatically share the host user's Docker credentials with Docklight. For private GHCR images managed by Docklight, keep a dedicated Docker config mounted into the container:

```yaml
volumes:
  - ./docker-config:/root/.docker
environment:
  DOCKER_CONFIG: /root/.docker
```

Then log in from the Docklight container:

```bash
docker exec -it docklight docker login ghcr.io -u <github-user>
```

Use a GitHub token with `read:packages`. This stores registry credentials under `./docker-config`, so protect that directory, do not commit it, and rotate the token if it is exposed. Deleting `./docker-config` removes Docklight's registry login. To remove the login without deleting the directory:

```bash
docker exec -it docklight docker logout ghcr.io
```

Do not copy a Docker Desktop `config.json` that relies on `credsStore` or `credHelpers`; those helper binaries usually are not available inside the Docklight container.

## Reverse Proxy Path

Default:

```yaml
BASE_URL: /
```

Path-based reverse proxy:

```yaml
BASE_URL: /docklight
```

Then use `http://host/docklight/`.

Set `COOKIE_SECURE: true` when serving Docklight only through HTTPS.

## Stored Data

SQLite stores durable database data only:

- discovered stack relative path
- compose filename
- sessions
- activity log

Docklight stores app settings, including activity retention and verbose logging state, in `/data/config.yml`.

Activity retention defaults to 30 days and can be changed on the Config page to 30, 60, 120, 180, or 365 days. Rows older than the selected limit are purged automatically. The Activity page can download all retained activity as CSV.

If you mount `./docker-config:/root/.docker` for private registry access, that folder is user-provided Docker CLI configuration and may contain registry credentials. Docklight does not manage those credentials.

Verbose logging is off by default. When enabled on the Config page, Docklight writes timestamped troubleshooting logs under `/data/logs` with daily filenames such as `docklight-verbose-2026-06-18.log`. Verbose log retention defaults to 30 days and supports the same 30, 60, 120, 180, or 365 day options. Out-of-retention verbose log files are deleted automatically, and retained historical log files can be downloaded from the Config page.

Docklight does not store container IDs or live Docker state. If the user changes containers with the Docker CLI, Docklight reads the live state again.

## Upgrade Actions

- `Pull + apply` runs `docker compose pull` and then `docker compose up -d`. Compose recreates services only when needed.
- `Pull + force upgrade` runs `docker compose pull`, `docker compose down --remove-orphans`, and then `docker compose up -d --force-recreate`. This restarts all services and should be used when a guaranteed recreation is needed.
- `Update Docklight` on the Config page starts a detached helper container that pulls and recreates only the running Docklight Compose service.

`Update Docklight` requires Docklight itself to be started by Docker Compose with the Docker socket mounted and the Docklight compose project directory mounted into the container at the same absolute path used on the host. Add a read-only bind mount for that project directory when you want to use this action:

```yaml
volumes:
  - /absolute/host/path/to/docklight:/absolute/host/path/to/docklight:ro
```
