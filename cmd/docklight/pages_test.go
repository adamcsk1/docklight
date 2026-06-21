package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCreateStackSuccessRedirectsToDashboard(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "started", nil
	}

	values := url.Values{}
	values.Set("name", "new-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("expected redirect to dashboard after successful create, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(root, "new-stack", "docker-compose.yml")); err != nil {
		t.Fatalf("expected docker-compose.yml to be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new-stack", "compose.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected compose.yml not to be written, stat error: %v", err)
	}
}

func TestDashboardHidesStackActionButtons(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "failed-stack"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('failed-stack', 'failed-stack', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/", nil)
	rec := httptest.NewRecorder()
	app.dashboard(rec, req)
	body := rec.Body.String()
	for _, removed := range []string{`/stacks/failed-stack/delete`, `>Delete</button>`, `>Start</button>`, `>Stop</button>`, `>Restart</button>`, `Pull + up`} {
		if strings.Contains(body, removed) {
			t.Fatalf("dashboard should not render stack action %q, body: %s", removed, body)
		}
	}
}

func TestDashboardHidesProtectedStackDeleteButton(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "docklight"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('docklight', 'docklight', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/", nil)
	rec := httptest.NewRecorder()
	app.dashboard(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, `/stacks/docklight/delete`) || !strings.Contains(body, `badge-muted">protected`) {
		t.Fatalf("expected protected dashboard row without delete button, body: %s", body)
	}
}

func TestDashboardRendersLazyUsagePlaceholderWithoutStats(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq":
			return "webcid\n", nil
		case "docker container inspect webcid":
			return `[{"ID":"webcid","Name":"/web-1","Image":"sha256:nginx","Config":{"Image":"nginx","Labels":{"com.docker.compose.project":"web","com.docker.compose.service":"web"}},"State":{"Status":"running"}}]`, nil
		}
		if strings.Contains(call, "compose --project-name web") && strings.Contains(call, " ps -a") {
			t.Fatalf("dashboard should not call docker compose ps during initial render: %s", call)
		}
		if strings.Contains(call, "stats --no-stream") {
			t.Fatalf("dashboard should not call docker stats during initial render: %s", call)
		}
		return "", nil
	}

	rec := httptest.NewRecorder()
	app.dashboard(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `/static/usage.js`) || !strings.Contains(body, `data-usage-url="/usage/containers"`) || !strings.Contains(body, `data-usage-key="web"`) || !strings.Contains(body, `data-usage-containers="web-1"`) || !strings.Contains(body, `CPU - / Memory -`) {
		t.Fatalf("expected lazy usage placeholders, body: %s", body)
	}
}

func TestDashboardLoadsLiveStatusWithSingleDockerInspect(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"api", "web"} {
		mustWriteCompose(t, filepath.Join(root, name))
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('api', 'api', 'compose.yml'), ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	listCalls := 0
	inspectCalls := 0
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq":
			listCalls++
			return "apicid\nwebcid\n", nil
		case "docker container inspect apicid webcid":
			inspectCalls++
			return `[
{"ID":"apicid","Name":"/api-1","Config":{"Image":"api:latest","Labels":{"com.docker.compose.project":"api","com.docker.compose.service":"api"}},"State":{"Status":"exited"}},
{"ID":"webcid","Name":"/web-1","Config":{"Image":"web:latest","Labels":{"com.docker.compose.project":"web","com.docker.compose.service":"web"}},"State":{"Status":"running"}}
]`, nil
		}
		if strings.Contains(call, " compose ") && strings.Contains(call, " ps ") {
			t.Fatalf("dashboard stack list should not call docker compose ps: %s", call)
		}
		return "", nil
	}

	stacks, err := app.loadStacks(true)
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 1 || inspectCalls != 1 {
		t.Fatalf("expected one docker list and inspect call, got list=%d inspect=%d", listCalls, inspectCalls)
	}
	statusByName := map[string]string{}
	countByName := map[string]int{}
	for _, stack := range stacks {
		statusByName[stack.Name] = stack.Status
		countByName[stack.Name] = stack.ContainerCount
	}
	if statusByName["api"] != "stopped" || countByName["api"] != 1 || statusByName["web"] != "running" || countByName["web"] != 1 {
		t.Fatalf("expected live status/counts from inspected containers, statuses=%v counts=%v", statusByName, countByName)
	}
}

func TestContainerUsageReturnsStackSummary(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if strings.Contains(call, "docker stats --no-stream") && strings.Contains(call, "db-1") && strings.Contains(call, "web-1") {
			return "abc123|web-1|1.25%|10MiB / 1GiB\ndef456|db-1|2.75%|22MiB / 1GiB\n", nil
		}
		return "", nil
	}

	rec := httptest.NewRecorder()
	app.containerUsage(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/usage/containers?target=web%7Cdb-1&target=web%7Cweb-1", nil))
	metrics := decodeUsageMetrics(t, rec)
	if metrics["web"].CPU != "4.00%" || metrics["web"].Memory != "32.0 MiB" {
		t.Fatalf("expected stack usage summary, got %#v", metrics)
	}
}

func TestStackDetailRendersLazyUsagePlaceholderWithoutStats(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if strings.Contains(call, "compose --project-name web") && strings.Contains(call, " ps -a --format json") {
			return `[
{"Name":"web-1","Service":"web","State":"running","Status":"Up","Image":"nginx"},
{"Name":"worker-1","Service":"worker","State":"exited","Status":"Exited","Image":"busybox"}
]`, nil
		}
		if strings.Contains(call, "stats --no-stream") {
			t.Fatalf("stack detail should not call docker stats during initial render: %s", call)
		}
		return "", nil
	}
	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/stacks/web", nil)
	req.SetPathValue("name", "web")
	rec := httptest.NewRecorder()
	app.stackDetail(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `/static/usage.js`) || !strings.Contains(body, `data-usage-url="/usage/containers"`) || !strings.Contains(body, `data-usage-key="web-1"`) || !strings.Contains(body, `data-usage-containers="web-1"`) || !strings.Contains(body, `CPU - / Memory -`) {
		t.Fatalf("expected lazy container usage placeholders, body: %s", body)
	}
}

func TestContainerUsageReturnsMetricsAndSkipsEmptyTargets(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if call == `docker stats --no-stream --format {{.Container}}|{{.Name}}|{{.CPUPerc}}|{{.MemUsage}} web-1` {
			return "abc123|web-1|1.25%|10MiB / 1GiB\n", nil
		}
		if strings.Contains(call, "worker-1") {
			t.Fatalf("stopped containers should not be requested from docker stats: %s", call)
		}
		return "", nil
	}
	rec := httptest.NewRecorder()
	app.containerUsage(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/usage/containers?target=web-1%7Cweb-1&target=worker-1%7C", nil))
	metrics := decodeUsageMetrics(t, rec)
	if metrics["web-1"].CPU != "1.25%" || metrics["web-1"].Memory != "10.0 MiB" {
		t.Fatalf("expected running container usage, got %#v", metrics)
	}
	if _, ok := metrics["worker-1"]; ok {
		t.Fatalf("empty target should be skipped, got %#v", metrics)
	}
}

func TestImagesPageShowsDeletionControls(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker image ls -q":
			return "img1\n", nil
		case "docker container ls -aq":
			return "", nil
		case "docker image inspect img1":
			return `[{"ID":"sha256:img1","RepoTags":["nginx:latest"],"RepoDigests":["nginx@sha256:abc"],"Size":1}]`, nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/images", nil)
	rec := httptest.NewRecorder()
	app.images(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `nginx:latest`) || !strings.Contains(body, `Delete selected`) || strings.Contains(body, `name="force"`) {
		t.Fatalf("expected image deletion controls, body: %s", body)
	}
	if strings.Contains(body, `/static/usage.js`) {
		t.Fatalf("images page should not load usage script, body: %s", body)
	}
}

func TestImagesPageShowsDeletionOnlyActions(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker image ls -q":
			return "current\navailable\n", nil
		case "docker container ls -aq":
			return "", nil
		case "docker image inspect current available":
			return `[{"ID":"sha256:current","RepoTags":["current:latest"],"RepoDigests":["current@sha256:abc"],"Size":1},{"ID":"sha256:available","RepoTags":["available:latest"],"RepoDigests":["available@sha256:def"],"Size":1}]`, nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/images", nil)
	rec := httptest.NewRecorder()
	app.images(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `Delete selected`) {
		t.Fatalf("expected selected delete button, body: %s", body)
	}
	if !strings.Contains(body, `current:latest`) || !strings.Contains(body, `available:latest`) {
		t.Fatalf("expected image inventory rows, body: %s", body)
	}
}

func TestImagesPageShowsManagedStackImageAsLockedInventory(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker image ls -q":
			return "managed\n", nil
		case "docker container ls -aq":
			return "managedcid\n", nil
		case "docker container inspect managedcid":
			return `[{"ID":"managedcid","Name":"/managed","Image":"sha256:managed","Config":{"Image":"managed:latest","Labels":{"com.docker.compose.project":"web","com.docker.compose.service":"app"}},"State":{"Status":"running"},"NetworkSettings":{"Ports":{},"Networks":{}}}]`, nil
		case "docker image inspect managed":
			return `[{"ID":"sha256:managed","RepoTags":["managed:latest"],"RepoDigests":["managed@sha256:old"],"Size":1}]`, nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/images", nil)
	rec := httptest.NewRecorder()
	app.images(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `value="managed"`) || !strings.Contains(body, `disabled title="locked"`) || !strings.Contains(body, `<span class="muted">locked: used by container</span>`) {
		t.Fatalf("expected managed image to be locked, body: %s", body)
	}
	if strings.Contains(body, `in use; deletion requires force`) {
		t.Fatalf("managed stack image should not show force-delete wording, body: %s", body)
	}
}

func TestContainersPageShowsRunningContainersAsLockedInventory(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq":
			return "runningcid\nstoppedcid\n", nil
		case "docker container inspect runningcid stoppedcid":
			return `[
{"ID":"runningcid","Name":"/running","Image":"sha256:running","Config":{"Image":"running:latest","Labels":{}},"State":{"Status":"running"},"NetworkSettings":{"Ports":{},"Networks":{}}},
{"ID":"stoppedcid","Name":"/stopped","Image":"sha256:stopped","Config":{"Image":"stopped:latest","Labels":{}},"State":{"Status":"exited"},"NetworkSettings":{"Ports":{},"Networks":{}}}
]`, nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/containers", nil)
	rec := httptest.NewRecorder()
	app.containers(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `value="runningcid"`) || !strings.Contains(body, `value="runningcid" disabled`) || !strings.Contains(body, `<p class="muted">locked: running container</p>`) {
		t.Fatalf("expected running container to be locked, body: %s", body)
	}
	if !strings.Contains(body, `value="stoppedcid"`) || strings.Contains(body, `value="stoppedcid" disabled`) {
		t.Fatalf("expected stopped container to remain selectable, body: %s", body)
	}
	if strings.Contains(body, `name="force"`) {
		t.Fatalf("containers page should not expose force deletion, body: %s", body)
	}
}

func TestPageHandlersReturnErrorsWhenBackendsFail(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		method func(http.ResponseWriter, *http.Request)
		url    string
		code   int
	}{
		{name: "dashboard", method: app.dashboard, url: "http://docklight.local/", code: http.StatusInternalServerError},
		{name: "activity", method: app.activity, url: "http://docklight.local/activity", code: http.StatusInternalServerError},
		{name: "stack detail", method: app.stackDetail, url: "http://docklight.local/stacks/missing", code: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			req.SetPathValue("name", "missing")
			rec := httptest.NewRecorder()
			tc.method(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("expected code %d, got %d", tc.code, rec.Code)
			}
		})
	}
}

func TestStandaloneExternalContainersPageShowsStandaloneExternalInventory(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq":
			return "standalonecid\nexternalcid\nmanagedcid\ndocklightcid\n", nil
		case "docker container inspect standalonecid externalcid managedcid docklightcid":
			return `[
{"ID":"standalonecid","Name":"/standalone","Image":"sha256:standalone","Config":{"Image":"standalone:latest","Labels":{}},"State":{"Status":"running"},"NetworkSettings":{"Ports":{},"Networks":{}}},
{"ID":"externalcid","Name":"/external","Image":"sha256:external","Config":{"Image":"external:latest","Labels":{"com.docker.compose.project":"other","com.docker.compose.service":"app"}},"State":{"Status":"running"},"NetworkSettings":{"Ports":{},"Networks":{}}},
{"ID":"managedcid","Name":"/managed","Image":"sha256:managed","Config":{"Image":"managed:latest","Labels":{"com.docker.compose.project":"web","com.docker.compose.service":"app"}},"State":{"Status":"running"},"NetworkSettings":{"Ports":{},"Networks":{}}},
{"ID":"docklightcid","Name":"/docklight","Image":"sha256:docklight","Config":{"Image":"docklight:latest","Labels":{"com.docker.compose.project":"docklight","com.docker.compose.service":"docklight"}},"State":{"Status":"running"},"NetworkSettings":{"Ports":{},"Networks":{}}}
]`, nil
		}
		if strings.Contains(call, "stats --no-stream") {
			t.Fatalf("external containers page should not call docker stats during initial render: %s", call)
		}
		return "", nil
	}
	rec := httptest.NewRecorder()
	app.standaloneExternalContainersPage(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/external-containers", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `Standalone / external containers`) || !strings.Contains(body, `standalone:latest`) || !strings.Contains(body, `external:latest`) || strings.Contains(body, `managed:latest`) {
		t.Fatalf("expected only standalone/external containers, body: %s", body)
	}
	if !strings.Contains(body, `protected: Docklight`) {
		t.Fatalf("expected protected Docklight row, body: %s", body)
	}
	if !strings.Contains(body, `/static/usage.js`) || !strings.Contains(body, `data-usage-url="/usage/containers"`) || !strings.Contains(body, `data-usage-key="externalcid"`) || !strings.Contains(body, `data-usage-containers="externalcid"`) || !strings.Contains(body, `CPU -<br>Memory -`) {
		t.Fatalf("expected lazy external container usage placeholders, body: %s", body)
	}
}

func TestExternalContainerUsageReturnsMetrics(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if strings.Contains(call, "docker stats --no-stream") && strings.Contains(call, "externalcid") && strings.Contains(call, "standaloneci") {
			return "standalonecid|standalone|0.50%|8MiB / 1GiB\nexternalcid|external|1.50%|16MiB / 1GiB\n", nil
		}
		return "", nil
	}

	rec := httptest.NewRecorder()
	app.containerUsage(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/usage/containers?target=externalcid%7Cexternalcid&target=standaloneci%7Cstandaloneci", nil))
	metrics := decodeUsageMetrics(t, rec)
	if metrics["externalcid"].CPU != "1.50%" || metrics["externalcid"].Memory != "16.0 MiB" || metrics["standaloneci"].CPU != "0.50%" {
		t.Fatalf("expected external usage metrics, got %#v", metrics)
	}
}

func TestContainerUsageCapsTargets(t *testing.T) {
	var values []string
	for i := 0; i < maxUsageTargets+10; i++ {
		values = append(values, "stack|container-"+strconv.Itoa(i))
	}
	groups := usageTargetGroups(values)
	if len(groups["stack"]) != maxUsageTargets {
		t.Fatalf("expected target cap %d, got %d", maxUsageTargets, len(groups["stack"]))
	}
}

func TestContainerUsageDeduplicatesTargets(t *testing.T) {
	groups := usageTargetGroups([]string{"stack|web-1", "stack|web-1", "stack|db-1"})
	if got := groups["stack"]; len(got) != 2 || got[0] != "web-1" || got[1] != "db-1" {
		t.Fatalf("expected duplicate targets to be ignored, got %#v", got)
	}
}

func decodeUsageMetrics(t *testing.T, rec *httptest.ResponseRecorder) map[string]usageMetric {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var metrics map[string]usageMetric
	if err := json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("decode usage metrics: %v body=%s", err, rec.Body.String())
	}
	return metrics
}
