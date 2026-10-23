# Docker deployment

## Local UI and agent

Use a Compose root such as `/srv/compose`, containing existing project folders and the Docklight project itself. Mount that root **at the identical absolute path inside the agent**. The agent uses Docker Compose labels to locate the original project directory, Compose files and service names. Identical paths preserve relative binds, `.env`, includes and extra configuration files without rewriting them.

Generate separate secrets with `openssl rand -hex 32` for the UI session secret and each agent token. In `/srv/compose/docklight/.env`, set:

```dotenv
ADMIN_PASSWORD=replace-with-a-strong-password
SESSION_SECRET=replace-with-a-random-secret-at-least-32-characters
AGENT_TOKEN=replace-with-a-different-random-secret-at-least-32-characters
```

Create `/srv/compose/docklight/ui-data/hosts.yml`, readable only by the user running Docklight:

```yaml
hosts:
  - id: local
    name: Local Docker
    url: http://docklight-agent:8081
    token: replace-with-the-same-AGENT_TOKEN
    allow_http: true # Only the private local Docker network; no published agent port.
```

Secrets in this YAML are literal; environment substitution is not performed. Mount it read-only and restart the UI after changing hosts. Host configuration intentionally stays in an administrator-owned file rather than exposing credentials through the browser.

Selecting a Docker host connects automatically when JavaScript is enabled. **Reconnect** reloads the selected host on demand; without JavaScript, use it to apply your selection. Submission, waiting-for-output, reconnecting and inventory-refresh status appear above the lists even when the operation output is hidden. Submission controls are locked while navigating so repeated clicks do not submit duplicate actions.

Save `/srv/compose/docklight/compose.yml`:

```yaml
services:
  docklight:
    image: ghcr.io/adamcsk1/docklight:latest
    labels:
      io.docklight.role: ui
    ports:
      - "127.0.0.1:7070:8080"
    environment:
      ADMIN_PASSWORD: ${ADMIN_PASSWORD:?required}
      SESSION_SECRET: ${SESSION_SECRET:?required}
      COOKIE_SECURE: "true" # Use HTTPS through a reverse proxy.
    volumes:
      - ./ui-data:/data
      - ./ui-data/hosts.yml:/data/hosts.yml:ro
    networks: [management]
    restart: unless-stopped
  docklight-agent:
    image: ghcr.io/adamcsk1/docklight-agent:latest
    labels:
      io.docklight.role: agent
    environment:
      AGENT_TOKEN: ${AGENT_TOKEN:?required}
      STACKS_ROOT: /srv/compose
      BUILDX_CONFIG: /data/buildx # Writable Buildx state, separate from registry credentials.
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /srv/compose:/srv/compose:ro
      - ./agent-data:/data
    networks: [management, outbound]
    restart: unless-stopped
networks:
  management:
    internal: true
  outbound: {}
```

The agent needs outbound access to pull images. The internal management network keeps its HTTP endpoint private. The UI has no Docker socket or Compose mount. For local HTTP testing only, set `COOKIE_SECURE: "false"`; otherwise use HTTPS at your reverse proxy.

Run `docker compose up -d` from the project directory. No automatic pulling/building occurs during start, stop or recreate actions. Newly created stacks must be deployed using the CLI first: Docklight manages existing containers, not unstarted Compose folders. A deleted container disappears from Docklight; deploy it again from the CLI if needed.

## Remote hosts and TLS

Run only `docklight-agent` on each remote host, with its own socket, identical-path Compose root mount and independent token. Publish its port only on a private interface/firewall allowlist reachable by the UI.

Set `TLS_CERT` and `TLS_KEY` to mounted PEM certificate/key paths. Both are required together. Use a certificate valid for the agent's DNS name or IP. Example host entry:

```yaml
  - id: pi
    name: Raspberry Pi
    url: https://pi.example.internal:8081
    token: independent-random-agent-token-at-least-32-characters
    ca_file: /data/certs/homelab-ca.pem # Optional private CA; public CAs work by default.
```

Alternatively, use HTTPS termination in a trusted reverse proxy, with an isolated connection from proxy to agent. Plain HTTP requires explicit `allow_http: true` and must be confined to an isolated local Docker network or encrypted VPN. Never send bearer tokens across an unencrypted LAN or the internet.

The UI refuses agent redirects, preventing credentials from being forwarded elsewhere. Keep host files, TLS keys, tokens and backups private. UI sessions expire after 24 hours; restarting with a different session secret invalidates them.

### Generate an agent certificate

For a private deployment without an existing certificate authority, generate a self-signed certificate using OpenSSL. Run these commands from the agent's Compose directory on Linux or inside WSL on Windows. For example, `C:\docker\docklight-agent` is `/mnt/c/docker/docklight-agent` in WSL. Replace the example DNS name and IP with the address(es) the UI will actually use to reach this agent, preferably through a VPN. The URL must match a Subject Alternative Name (SAN); setting only the certificate's common name is not sufficient.

```bash
umask 077
mkdir -p certs
openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 365 \
  -keyout certs/agent.key \
  -out certs/agent.crt \
  -subj "/CN=docklight-agent" \
  -addext "subjectAltName=DNS:pi.example.internal,IP:192.0.2.10" \
  -addext "basicConstraints=critical,CA:TRUE"
```

Merge these settings into the existing agent service, keeping its token, socket, stack-root and data configuration:

```yaml
environment:
  TLS_CERT: /certs/agent.crt
  TLS_KEY: /certs/agent.key
volumes:
  - ./certs:/certs:ro
```

Copy **only `agent.crt`**, over a trusted connection, to the UI host as `./ui-data/certs/pi-agent.crt` (using the UI data mount from the example above). In the UI's `hosts.yml`, set the corresponding host's `url` to `https://pi.example.internal:8081` (or the IP included in the SAN), and `ca_file` to `/data/certs/pi-agent.crt`. The `ca_file` path is inside the UI container, not the agent. Never copy the private `agent.key` to the UI or commit it to Git.

On the agent host, apply the TLS configuration with `docker compose up -d --force-recreate docklight-agent`. On the UI host, run `docker compose restart docklight` after installing the trusted certificate and updating `hosts.yml`.

The certificate expires after one year. Renew it before expiry and replace it if the connection address changes to one not listed in the SAN; update the UI's trusted certificate and restart both components when replacing it. On Windows-mounted directories, also restrict the private key using Windows file permissions; WSL `umask` alone does not guarantee restrictive NTFS permissions. TLS encrypts and authenticates the connection but does not replace the agent token, VPN or firewall restrictions.

## Updates and private registries

### Locally built application images

**Build** appears for application services with a resolved Compose `build:` section. It runs `docker compose build` for the selected service (all replicas), or the existing buildable services in the selected stack. Only after a successful build does it run `up -d --no-deps --no-build --pull never` for those services. A failed build leaves existing containers running. Deployment failures after a successful build can still affect availability.

Build uses normal Docker caching; it does not force a base-image pull, disable cache, deploy dependencies or clean build cache. The agent needs access to the build context, Dockerfile and any referenced files, normally through the same-path read-only stack mount. SSH forwarding and extra build-secret mounts are not configured by Docklight. Build output is retained with the operation and may contain anything printed by Dockerfile commands; avoid printing secrets.

**Upgrade** pulls and deploys only services without `build:`. In mixed stacks, local services are left untouched. A service containing both `image:` and `build:` uses Build, not Upgrade. Missing or invalid Compose configuration disables build discovery with an explanation; resolved configuration and its diagnostics are not exposed through inventory. Build discovery is reused within each project's inventory request.

Build jobs allow two hours instead of the usual ten minutes. Mutations remain serialized per host while a build runs. Build does not apply to protected management containers: their dedicated Update action remains registry-based.

UI and agent image labels protect management containers from ordinary actions. Compose deployments should also set the role labels explicitly as above. Dedicated **Update** actions pull and recreate only the selected UI/agent service through a detached helper. Helpers use the current agent image ID and inherited read-only mounts; they continue running if the agent/UI is replaced. Results are read from the helper after reconnection.

Keep the Docklight deployment and its `.env` within the agent's mounted root. Runtime-only shell variables are not inherited by update helpers; put required Compose substitutions in the project's `.env` or files referenced by Compose. Use matching release tags for UI and agent. Pinned image digests do not advance automatically; edit the Compose configuration outside Docklight to change them.

Custom Compose `--env-file` settings are replayed from container labels, in their original order, for discovery, recreation, builds, upgrades and management updates. These files must remain accessible at their labeled absolute paths within `STACKS_ROOT`; missing files or symlink escapes disable Compose operations rather than silently falling back to `.env`.

For private registries, mount a dedicated Docker CLI configuration into the agent at `/root/.docker:ro`. Helpers inherit this mount. Authenticate that configuration beforehand with `docker --config <directory> login <registry>`. Desktop credential-helper configurations will not work unless the helper exists inside the agent image. Do not mount unrelated host credentials.

Keep `BUILDX_CONFIG: /data/buildx` in both local and remote agent environments. Buildx writes builder state and activity timestamps, so its directory must be writable even when `/root/.docker` is mounted read-only for registry credentials. The existing agent `/data` mount provides persistent writable storage; do not make the credentials mount writable. After changing the environment, recreate the agent with `docker compose up -d docklight-agent` before retrying Build.

After an update finishes, the agent saves its helper logs in the job result, then removes the exited helper so old images can become unused. If cleanup fails or no client reconnects to collect the result, inspect the helper manually (`docker logs docklight-update-...`, then `docker rm docklight-update-...` after it exits). Helpers have `io.docklight.role=updater` and are protected from ordinary Docklight deletion. Cleanup verifies ownership labels and never force-removes a helper.

## Storage and recovery

- UI `/data`: `sessions.db` for signed login sessions, host configuration and optional CA certificates.
- Agent `/data`: persisted jobs/results (latest 100 completed operations), temporary image overrides and Buildx state when `BUILDX_CONFIG=/data/buildx`.
- Original Compose files and application volumes are never edited/deleted by Docklight.

There is no Activity menu or operation-history list. After submitting an action, its result page shows progress, output and errors. Keep that URL to revisit the result. Agents retain bounded job records for result retrieval, restart recovery and management-update safety; removing the history UI does not remove these safeguards or delete existing records. Login failures and submitted-operation receipts are written to UI container logs.

With JavaScript enabled, running operations refresh their status and output every three seconds. The output box starts at the end and scrolls to new output automatically. Temporary agent disconnects are retried without reloading or resubmitting the operation. On success or failure, container/resource lists refresh in place, with three checks spaced three seconds apart to catch delayed Docker state changes. Failed inventory requests keep the existing lists visible and show a retry/error message in the output panel. Accordion choices and hidden-output state are preserved. Without JavaScript, reload manually to see progress or current inventory.

The history-list API (`GET /v1/jobs`) has also been removed. Update the UI and agents together: older UIs still request that endpoint. Submitting operations (`POST /v1/jobs`) and retrieving a specific result (`GET /v1/jobs/{id}`) remain supported.

The UI database contains only session hashes and expiry times, with no usernames, stack settings or activity history. There are no database migrations or automatic backups. Only the current session schema is accepted. If an explicit `DB_PATH` points to an incompatible database, startup refuses it without modifying its contents; select a fresh database path instead. The default `/data/sessions.db` does not read or replace other database files. Changing session databases requires signing in again and does not affect agent job history, host configuration or Docker resources.

Operations continue after a browser/UI disconnect. Agent restart marks unfinished ordinary jobs failed with an **unknown outcome** warning; it does not retry destructive operations. Update jobs reconnect to their detached helper. While a management update is unresolved, further mutations are blocked. If a helper was manually removed or an agent died before launching it, inspect Docker manually; stop the agent, back up its data, remove the affected `job-<uuid>.json`, then restart. Do not clear uncertain jobs without checking the host.

Only one agent instance may use a given data directory and Docker daemon for mutations. Docklight serializes its own actions, not commands issued by external Docker clients. Resource deletion rechecks references and uses Docker's non-force deletion as the final guard; external users can still race an operation.

## Environment

UI: `ADDR` (`:8080`), `BASE_URL` (`/`), required `ADMIN_PASSWORD`, `SESSION_SECRET`, `DB_PATH` (`/data/sessions.db`), `HOSTS_FILE` (`/data/hosts.yml`), `COOKIE_SECURE` (`false`). Login is password-only.

Agent: `ADDR` (`:8081`), required `AGENT_TOKEN`, `STACKS_ROOT` (`/stacks`), `DATA_DIR` (`/data`), optional paired `TLS_CERT`/`TLS_KEY`. Operations have a ten-minute timeout, except Build (two hours). Compose paths must be absolute and resolve within `STACKS_ROOT`; symlink escapes are rejected.

For Windows Docker Desktop development, use the development example and WSL/Linux-side Compose project paths for Compose management. Linux-style paths in Docker labels must be accessible inside the Linux agent. Arbitrary Windows-native host path translation is not supported by the identical-path model.
