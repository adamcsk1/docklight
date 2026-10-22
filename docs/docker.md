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

## Updates and private registries

UI and agent image labels protect management containers from ordinary actions. Compose deployments should also set the role labels explicitly as above. Dedicated **Update** actions pull and recreate only the selected UI/agent service through a detached helper. Helpers use the current agent image ID and inherited read-only mounts; they continue running if the agent/UI is replaced. Results are read from the helper after reconnection.

Keep the Docklight deployment and its `.env` within the agent's mounted root. Runtime-only shell variables are not inherited by update helpers; put required Compose substitutions in the project's `.env` or files referenced by Compose. Use matching release tags for UI and agent. Pinned image digests do not advance automatically; edit the Compose configuration outside Docklight to change them.

For private registries, mount a dedicated Docker CLI configuration into the agent at `/root/.docker:ro`. Helpers inherit this mount. Authenticate that configuration beforehand with `docker --config <directory> login <registry>`. Desktop credential-helper configurations will not work unless the helper exists inside the agent image. Do not mount unrelated host credentials.

After an update finishes, the agent saves its helper logs in the job result, then removes the exited helper so old images can become unused. If cleanup fails or no client reconnects to collect the result, inspect the helper manually (`docker logs docklight-update-...`, then `docker rm docklight-update-...` after it exits). Helpers have `io.docklight.role=updater` and are protected from ordinary Docklight deletion. Cleanup verifies ownership labels and never force-removes a helper.

## Storage and recovery

- UI `/data`: SQLite sessions, UI audit history, host configuration and optional CA certificates.
- Agent `/data`: persisted jobs/results (latest 100 completed operations), temporary image overrides.
- Original Compose files and application volumes are never edited/deleted by Docklight.

Operations continue after a browser/UI disconnect. Agent restart marks unfinished ordinary jobs failed with an **unknown outcome** warning; it does not retry destructive operations. Update jobs reconnect to their detached helper. While a management update is unresolved, further mutations are blocked. If a helper was manually removed or an agent died before launching it, inspect Docker manually; stop the agent, back up its data, remove the affected `job-<uuid>.json`, then restart. Do not clear uncertain jobs without checking the host.

Only one agent instance may use a given data directory and Docker daemon for mutations. Docklight serializes its own actions, not commands issued by external Docker clients. Resource deletion rechecks references and uses Docker's non-force deletion as the final guard; external users can still race an operation.

## Migrating the former single-container app

1. Back up the existing data folder and Compose files. Stop the old Docklight container; do not remove volumes.
2. Replace its deployment with the UI + agent layout. Mount the existing data folder into the UI and a **new separate data folder** into the agent.
3. Add `hosts.yml`, generate an agent token, and ensure `SESSION_SECRET` has at least 32 characters.
4. Mount the host Compose root at identical absolute paths in the agent. Previous `STACKS_HOST_ROOT` rewriting is no longer used.
5. Start both services and verify inventory before performing mutations.

Before migrating an existing database, Docklight creates `<DB_PATH>.pre-agent.bak` once. Existing stack metadata and activity history remain in SQLite; retired file-management settings are no longer acted on. Legacy plaintext sessions are invalidated. The old `config.yml` and stack-local managed configuration are left untouched but ignored. Existing filesystem data is not migrated, rewritten or removed.

## Environment

UI: `ADDR` (`:8080`), `BASE_URL` (`/`), required `ADMIN_PASSWORD`, `SESSION_SECRET`, `DB_PATH` (`/data/docklight.db`), `HOSTS_FILE` (`/data/hosts.yml`), `COOKIE_SECURE` (`false`). Login is password-only; `ADMIN_USER` is no longer used. Legacy sessions for a custom username require signing in again.

Agent: `ADDR` (`:8081`), required `AGENT_TOKEN`, `STACKS_ROOT` (`/stacks`), `DATA_DIR` (`/data`), optional paired `TLS_CERT`/`TLS_KEY`. Docker commands have a ten-minute timeout. Compose paths must be absolute and resolve within `STACKS_ROOT`; symlink escapes are rejected.

For Windows Docker Desktop development, use the development example and WSL/Linux-side Compose project paths for Compose management. Linux-style paths in Docker labels must be accessible inside the Linux agent. Arbitrary Windows-native host path translation is not supported by the identical-path model.
