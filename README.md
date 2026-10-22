# Docklight

Small, single-admin Docker manager for a home lab. One password-protected web UI connects to a management agent on each Docker host, including the local machine.

## Scope

- Start, stop and delete containers, individually or by Compose service/stack.
- Show containers in Compose-stack groups with running/total counts and a separate standalone group. Groups start collapsed; browser-local storage remembers each host/stack's expanded or collapsed state across reloads.
- Recreate Compose services using the **exact image IDs currently in use**, without pulling or building.
- Upgrade Compose services with `docker compose pull` followed by `docker compose up -d`. A failed pull prevents the up step.
- Delete selected unused images, volumes and networks. Referenced resources remain read-only, including references from stopped containers.
- Update Docklight and its agents through detached helpers.
- Show progress and errors for the submitted operation, without an Activity menu or history list.

Standalone containers support start, stop and delete only. Docklight does not edit Compose files, create stacks, provide a terminal, or monitor CPU/memory.

## Deployment

Published images:

```text
ghcr.io/adamcsk1/docklight
ghcr.io/adamcsk1/docklight-agent
```

Both images target `linux/amd64`, `linux/arm64` and `linux/arm/v7`. Raspberry Pi Zero 2 W, Pi 4 and Pi 5 are supported architectures; **64-bit Raspberry Pi OS is recommended**. The original Pi Zero/Zero W (ARMv6) is not supported. A Zero 2 W has only 512 MB RAM: prefer running just the agent there, with the UI on another machine. Architecture builds and emulated smoke tests do not replace testing on physical hardware.

See [deployment instructions](docs/docker.md) for a complete example, TLS, remote hosts, updates and storage. See [development](docs/development.md) for tests and publishing.

## Security

The UI requires only an admin password (`ADMIN_PASSWORD`) to sign in, plus `SESSION_SECRET` (at least 32 characters) for session protection. It retains signed, expiring sessions, CSRF protection and login throttling. Put the UI behind HTTPS; do not expose either component directly to the public internet.

Every agent requires its own random `AGENT_TOKEN` of at least 32 characters. Treat it as a host administrator credential: access to the Docker socket is effectively root access. Tokens stay in the UI's server-side host configuration, never in browser requests or URLs.

Remote agent connections require verified HTTPS unless explicitly configured for an isolated local network or encrypted VPN. TLS verification cannot be disabled. An isolated local Docker network can use HTTP without publishing the agent port. A token over unencrypted public/LAN HTTP is not secure.

Deletion is deliberate: stop containers first, then explicitly confirm deletion. Container removal does not delete volumes or Compose files. Deleting an unused volume permanently destroys its data; Docker considers a volume unused even if a stopped/deleted application may need it later.
