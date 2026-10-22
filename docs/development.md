# Development

Docklight uses Go, server-rendered HTML and plain CSS. No Node/npm build or frontend runtime dependency is required.

```sh
go test ./...
go test -race -cover ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go build ./cmd/docklight
go build ./cmd/docklight-agent
docker build --target ui -t docklight:dev .
docker build --target agent -t docklight-agent:dev .
```

Tests cover admin authentication/session expiry/throttling, CSRF/origin checks, per-host routing and token secrecy, TLS/redirect rejection, exact-image recreation, failed-pull behavior, mutation serialization, job restart recovery, protected containers and referenced-resource deletion. Removed feature tests are replaced with tests for the smaller API and UI; a passing suite is not proof that all operational failures are impossible.

Use a fully patched supported Go toolchain for local builds. The Dockerfile and CI track the latest Go 1.26 patch release; a newer minor version with an old patch level can still contain known security flaws. CI also checks reachable Go vulnerabilities before publishing. This does not replace scanning runtime OS/Docker CLI dependencies or testing deployment security.

## Isolated Docker integration tests

Never run destructive integration tests against a real host. Create a disposable nested daemon:

```sh
docker run -d --privileged --hostname docklight-isolated-test --name docklight-test-daemon \
  -e DOCKER_TLS_CERTDIR= -p 127.0.0.1:23750:2375 docker:29-dind
# Wait until this succeeds:
docker --host tcp://127.0.0.1:23750 info
DOCKLIGHT_TEST_DOCKER_HOST=tcp://127.0.0.1:23750 go test -v ./internal/management -run TestDockerIntegration -count=1
docker rm -fv docklight-test-daemon
```

The test refuses a daemon whose hostname is not `docklight-isolated-test`. It creates a Compose service and volume inside that daemon, moves a mutable image tag, checks that recreate keeps the old image, and verifies stopped-container volume protection plus volume preservation on container deletion. The nested daemon has no host Docker socket mounted.

To also exercise actual UI/agent replacement and job recovery, build the native images as `docklight:refactor` and `docklight-agent:refactor` first. Set `DOCKLIGHT_TEST_OUTER_CONTAINER=docklight-test-daemon` along with the endpoint, then run `go test -v ./internal/management -run TestDocker -count=1 -timeout=10m`. This imports those images into the disposable daemon, creates a local throwaway registry, publishes changed images and checks that both management containers are replaced successfully. CI runs both integration tests before multi-platform publishing.

## Multi-platform publishing

CI runs unit/race tests, vet and isolated Docker integration tests first. It then builds both images for `linux/amd64`, `linux/arm64`, and `linux/arm/v7`, smoke-testing the binaries and the agent's Compose plugin under emulation. Publishing is blocked if any target fails.

Pushes publish matching tags to `ghcr.io/<owner>/docklight` and `ghcr.io/<owner>/docklight-agent`: `latest` on the default branch, `sha-<commit>`, and semantic version tags from `v*` Git tags. Pull requests build/test but do not publish. Make both GHCR packages public if anonymous pulls are required.

The Dockerfile cross-compiles Go binaries on the build platform and uses multi-architecture runtime bases. ARMv7 uses `GOARM=7`. Raspberry Pi Zero 2 W/4/5 can use ARM64; original ARMv6-only Pi Zero cannot. Emulated smoke tests do not establish physical-device reliability, memory suitability or SD-card durability.

For manual builds:

```sh
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 --target ui -t <registry>/docklight:<tag> --push .
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 --target agent -t <registry>/docklight-agent:<tag> --push .
```
