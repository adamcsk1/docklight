# Development

This file keeps maintainer details out of the user-facing README.

Deployment and Docker runtime details live in [Docker Deployment](docker.md).

## Published Image

CI publishes Docklight to GitHub Container Registry after tests pass on pushed branches and version tags:

```text
ghcr.io/adamcsk1/docklight
```

The publish job runs only for `push` events and depends on the test job. Pull requests run tests but do not publish images.

Image tags:

- `latest` tracks the default branch.
- `sha-<commit>` is published for every pushed commit and is immutable for that commit.
- `v1.2.3`, `1.2.3`, `1.2`, and `1` are published from Git tags such as `v1.2.3`.

If the GHCR package should allow unauthenticated pulls, make it public in GitHub Packages after the first image is published.

For private GHCR packages, host-side `docker login ghcr.io` is required before Docker can pull the Docklight image itself. Docklight image pulls run Docker CLI commands inside the Docklight container through the mounted Docker socket, so they need credentials in the container's Docker config path.

The example compose file mounts a dedicated config directory for this:

```yaml
volumes:
  - ./data/docker-config:/root/.docker
environment:
  DOCKER_CONFIG: /root/.docker
```

Authenticate that config from inside the running container:

```sh
docker exec -it docklight docker login ghcr.io -u <github-user>
```

Use a least-privileged GitHub token with `read:packages`. Do not copy a Docker Desktop `config.json` that relies on `credsStore` or `credHelpers`; those helper binaries usually are not available inside the Docklight container. Treat `./data/docker-config/config.json` as secret material and rotate the token if it is exposed.

## Vendored Frontend Dependency

HTMX is vendored at `cmd/docklight/static/htmx.min.js` so Docklight does not depend on a CDN at runtime. The vendored version is tracked in `cmd/docklight/static/htmx.version`.

Current vendored version:

```text
htmx.org 2.0.10
```

Upgrade HTMX manually with:

```sh
version=2.0.10
curl -fsSL "https://unpkg.com/htmx.org@$version/dist/htmx.min.js" -o cmd/docklight/static/htmx.min.js
printf '%s\n' "$version" > cmd/docklight/static/htmx.version
```

When needed, manually check that the tracked version matches the vendored file:

```sh
expected="$(tr -d '[:space:]' < cmd/docklight/static/htmx.version)"
actual="$(sed -n 's/.*version:"\([^"]*\)".*/\1/p' cmd/docklight/static/htmx.min.js | head -n 1)"
test "$actual" = "$expected"
```

To compare with the latest upstream release, inspect:

```sh
curl -fsSL https://registry.npmjs.org/htmx.org/latest
```

## Go Dependency Updates

Go package update checks are manual. CI verifies that the current dependency set builds and tests, but it does not fail only because newer module versions exist.

Check available module updates with:

```sh
go list -u -m all
```

Update direct and retained indirect dependencies deliberately:

```sh
go get -u ./...
go mod tidy
```

After dependency changes, verify the project with:

```sh
go test ./...
go test ./... -cover
go vet ./...
```

Review `go.mod` and `go.sum` before committing. Do not force updates for modules that `go mod tidy` removes or does not retain.
