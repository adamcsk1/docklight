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

Mount a dedicated config directory for this in the Docklight Compose service:

```yaml
volumes:
  - ./docker-config:/root/.docker
environment:
  DOCKER_CONFIG: /root/.docker
```

Authenticate that config from inside the running container:

```sh
docker exec -it docklight docker login ghcr.io -u <github-user>
```

Use a least-privileged GitHub token with `read:packages`. Do not copy a Docker Desktop `config.json` that relies on `credsStore` or `credHelpers`; those helper binaries usually are not available inside the Docklight container. Treat `./docker-config/config.json` as secret material and rotate the token if it is exposed.

## Frontend Dependency

HTMX is managed through npm and generated into `cmd/docklight/static/htmx.min.js` before Go builds. The generated file is ignored by git, but Go embeds it into the Docklight binary so runtime does not depend on npm or a CDN.

Current version:

```text
htmx.org 2.0.10
```

Prepare local builds and tests with:

```sh
npm ci
npm run sync:htmx
```

Then run Go commands normally:

```sh
npm test
npm run build
```

The Docker build runs the same npm sync step before `go build`.

Upgrade HTMX with an exact dependency version:

```sh
npm install --save-exact htmx.org@<version>
npm run sync:htmx
go test ./...
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
npm ci
npm run sync:htmx
npm test
go test ./... -cover
go vet ./...
```

Review `go.mod` and `go.sum` before committing. Do not force updates for modules that `go mod tidy` removes or does not retain.
