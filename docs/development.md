# Development

Docklight uses Go, server-rendered HTML and plain CSS. No Node/npm build or frontend runtime dependency is required.

```sh
go test ./...
go test -race -cover ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
go build ./cmd/docklight
go build ./cmd/docklight-agent
docker build --build-arg BUILD_COMMIT="$(git rev-parse HEAD)" --target ui -t docklight:dev .
docker build --build-arg BUILD_COMMIT="$(git rev-parse HEAD)" --target agent -t docklight-agent:dev .
```

Tests cover admin authentication/session expiry/throttling, CSRF/origin checks, per-host routing and token secrecy, TLS/redirect rejection, exact-image recreation, failed-pull behavior, mutation serialization, job restart recovery, protected containers and referenced-resource deletion. Health tests cover minimal unauthenticated responses, security headers, the agent's exact-route authentication bypass, Dockerfile wiring and HTTP/TLS probe behavior. Maintenance tests cover master conflicts, atomic claims, lease expiry with active helpers, replay-safe jobs, persistence failures, partial updates, retries, lost responses, service-health verification and coordinator restart. Removed feature tests are replaced with tests for the smaller API and UI; a passing suite is not proof that all operational failures are impossible.

Symlink regressions cover Compose files, project directories and custom environment files, including chained escapes and valid links within the root. Linux tests fail rather than skip if symlinks are unavailable. On Windows, enable Developer Mode with administrator approval, or run the tests locally in a Linux container. Set `DOCKLIGHT_REQUIRE_SYMLINKS=1` to prohibit skips; CI enforces this explicitly.

Use a fully patched supported Go toolchain for local builds. The module and Dockerfile require Go 1.27.4; CI tracks the latest Go 1.27 patch release. With the default `GOTOOLCHAIN=auto`, Go downloads the required toolchain when necessary. Update the Dockerfile pin alongside the module minimum to avoid stale cached minor-version tags. A newer minor version with an old patch level can still contain known security flaws. CI also checks reachable Go vulnerabilities before publishing. This does not replace scanning runtime OS/Docker CLI dependencies or testing deployment security.

## Browser presentation checks

The UI retains Docklight's original dark monospace palette, resource grids, buttons and badges. Containers, cleanup and maintenance have separate screens. Containers are grouped by Compose project in native collapsible sections, collapsed by default, with running/total counts and a separate standalone group. Secondary actions use a nested disclosure; target selection stays visible so primary actions cannot conceal a whole-stack scope. Mobile controls have a minimum height of 44 pixels. Groups and containers are sorted by name. A small external script remembers stack accordion state in local storage, scoped by base URL, host and stack. If JavaScript or browser storage is disabled, the native controls still work without persistence. Navigation remains visible on mobile. The Content Security Policy permits same-origin scripts, CSS and images, but no inline code/styles or evaluation.

CI uses Playwright as an isolated test tool, not an application dependency. The browser test starts a temporary UI with a fake agent; it never calls Docker. It checks login, stack grouping/counts and keyboard collapse, host selection, protected/read-only actions, confirmation, operation output escaping and logout at 1440, 900, 390 and 320 pixel widths, both at `/` and `/docklight`, with JavaScript disabled. It also checks overflow, original design colors, section navigation and browser/CSP errors. Go tests cover the session-only database schema, persisted sessions and refusal to modify incompatible databases.

Additional JavaScript-enabled checks verify collapsed defaults, open/closed state restoration, host/stack isolation, standalone state and resize/reload persistence. Malformed stored values are ignored, and disabled local storage must not cause browser errors or disable native accordions. Only presentation preferences go into local storage; authentication remains in the HttpOnly session cookie and server-side session database.

On a dashboard request without a job ID, `GET /v1/jobs/current` restores the selected agent's active job, or its newest retained completed job. The response contains no coordinator credentials or operation history. Older agents without this endpoint still support explicit job links. Operation polling uses an authenticated same-origin JSON endpoint that fetches only the selected host's job. On completion, a separate authenticated endpoint renders container/resource fragments using the same templates as the dashboard. Three sequential inventory checks, spaced three seconds apart, catch delayed Docker changes without navigating the page. Refresh pauses while an inventory form has focus, preserving edits, and retains expanded stack/action disclosures. Dashboard requests read job status before inventory to avoid rendering a completed job beside an earlier snapshot. JavaScript-disabled pages offer manual refresh instead of timed navigation.

Fleet verification runs in the background and publishes a timestamped snapshot. Dashboard, maintenance polling and inventory reads do not trigger fleet checks or wait on the coordinator mutex. Reads remain available during conflicts; mutation controls are hidden. Ordinary submissions check durable conflict/run state, then rely on the target agent's atomic ownership/reservation checks rather than waiting for unrelated hosts. Coordinated maintenance still performs full preflight verification. Its durable scheduler remains in the UI service, not the agent.

Each action form carries a UUID request identity. Updated agents persist it as the job ID and reject reuse for a different action or owner; a repeated accepted request returns the original job. This protection lasts while the job is retained (up to 100 completed jobs plus active jobs and protected maintenance jobs). Submission acceptance uses a bounded context independent of browser cancellation; execution already uses the agent's independent context. Focused regressions cover a cancelled request during Compose pull, lost responses, replay, and rediscovery after UI and agent restart. An actual agent process crash is different: ordinary interrupted jobs are marked failed with an unknown-outcome warning, not automatically rerun.

Close output hides the operation panel without cancelling its job or polling. Show output restores it, including the latest log. The URL records the hidden state so manual reloads keep it hidden; completion updates leave the panel untouched, while a newly submitted operation opens its own output. Both controls work as ordinary links without JavaScript and update in place when JavaScript is enabled.

With JavaScript enabled, selecting a Docker host submits the host form automatically; Reconnect reloads the selected host explicitly. Host navigation and mutation forms paint an immediate status message before native submission and lock duplicate mutations and stale-host action buttons. Host navigation remains enabled during mutation acceptance. Disabled submitter/host values are preserved as hidden fields. Native validation still runs before locking controls, and the server continues to enforce authentication, CSRF and confirmation. A status line outside the output panel shows waiting, reconnecting and inventory-refresh progress even when output is hidden. Browser checks hold native submissions before navigation to verify feedback, then release real form submissions to verify payloads and host selection.

Session regression checks also resize and reload the same authenticated browser context, switch mobile device metrics and user agent, then restore desktop mode. The session cookie must remain unchanged. Opening `/login` with a valid session must redirect to the dashboard rather than show a login form; expired, tampered, revoked or unavailable sessions must still require authentication.

To run locally on Linux:

```sh
npm install --prefix /tmp/docklight-browser --no-save --no-package-lock playwright@1.65.1
/tmp/docklight-browser/node_modules/.bin/playwright install --with-deps chromium
NODE_PATH=/tmp/docklight-browser/node_modules DOCKLIGHT_UI_BROWSER_TEST=1 \
  go test -v ./cmd/docklight -run TestBrowserPresentation -count=1 -timeout=5m
```

Optionally set `PLAYWRIGHT_BROWSER_CHANNEL=chrome` to use an installed Chrome browser, and `DOCKLIGHT_UI_SCREENSHOT_DIR` to retain screenshots outside the repository. Normal Go tests and Docker builds do not require Node or Playwright.

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

To also exercise actual UI/agent replacement and job recovery, build the native images as `docklight:refactor` and `docklight-agent:refactor` first. Set `DOCKLIGHT_TEST_OUTER_CONTAINER=docklight-test-daemon` along with the endpoint, then run `go test -v ./internal/management -run TestDocker -count=1 -timeout=12m`. This imports those images into the disposable daemon, creates a local throwaway registry, publishes changed images and checks that both management containers are replaced successfully. The `ordinary-upgrade-ui-crash` subtest pauses that registry to hold a real Compose upgrade, kills the UI with SIGKILL, and verifies deployment completes while the UI remains stopped. It then starts the UI with its existing data, restores the session/job/inventory without a job URL, and replays the original request without creating another job. This is a real-container HTTP test; browser recovery tests separately use fake agents. Its `image-health-checks` subtest verifies inherited health configuration and live probes, including unset, `/` and `/docklight` UI base paths, a custom UI listen port, accepted noncanonical base-path forms, and agent HTTP plus Docker socket access. CI runs both integration tests before multi-platform publishing.

## Multi-platform publishing

CI runs unit/race tests, vet, browser checks and isolated Docker integration tests on every run. The integration tests still build local, throwaway UI and agent images. Production multi-platform builds and publishing select only affected targets: `cmd/docklight/` selects UI, `cmd/docklight-agent/` selects agent, and shared code, dependencies, Docker/build configuration or unknown inputs select both. Docs and known development-only files select neither. Release tags and missing comparison history select both. Pushes compare the entire pushed range; pull requests compare their head against the merge base. Selection logic has standard-library Python tests (`python3 -m unittest discover -s scripts -p test_ci_targets.py`).

Selected images build for `linux/amd64`, `linux/arm64`, and `linux/arm/v7`, smoke-testing the binaries and the agent's Compose/Buildx plugins under emulation. Publishing is blocked if tests or any selected target fail. Docs-only changes skip production image jobs without skipping tests.

Path detection does not notice updated base images in external registries. Change the Dockerfile or push a release tag when both images need rebuilding for updated bases.

Pushes publish selected images to `ghcr.io/<owner>/docklight` and/or `ghcr.io/<owner>/docklight-agent`: `latest` on the default branch, `sha-<commit>`, and semantic version tags from `v*` Git tags. An unchanged image retains its existing tags; it does not receive a new commit tag. Release tags publish both images with matching version tags. Pull requests build/test selected targets but do not publish. Make both GHCR packages public if anonymous pulls are required.

The Dockerfile cross-compiles Go binaries on the build platform and uses multi-architecture runtime bases. ARMv7 uses `GOARM=7`. Raspberry Pi Zero 2 W/4/5 can use ARM64; original ARMv6-only Pi Zero cannot. Emulated smoke tests do not establish physical-device reliability, memory suitability or SD-card durability.

For manual builds:

```sh
docker buildx build --build-arg BUILD_COMMIT="$(git rev-parse HEAD)" --platform linux/amd64,linux/arm64,linux/arm/v7 --target ui -t <registry>/docklight:<tag> --push .
docker buildx build --build-arg BUILD_COMMIT="$(git rev-parse HEAD)" --platform linux/amd64,linux/arm64,linux/arm/v7 --target agent -t <registry>/docklight-agent:<tag> --push .
```
