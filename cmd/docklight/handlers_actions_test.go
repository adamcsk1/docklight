package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteSelectedContainers(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/web","Image":"sha256:image","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"exited","StartedAt":""},"NetworkSettings":{"Ports":{},"Networks":{}}}]`, nil
		case "docker container rm abc123456789":
			return "ok", nil
		}
		return "", nil
	}

	req := formRequest("http://docklight.local/containers/delete", url.Values{"ids": {"abc123456789"}})
	rec := httptest.NewRecorder()
	app.deleteSelectedContainers(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(strings.Join(calls, "\n"), "container rm abc123456789") {
		t.Fatalf("expected delete wrapper to remove container, code=%d calls=%v", rec.Code, calls)
	}
}

func TestContainerLogsHandlers(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/web","Image":"sha256:image","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{}}}]`, nil
		case "docker logs --tail 300 abc123456789":
			return "first line\nsecond line", nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/containers/abc123456789/logs", nil)
	req.SetPathValue("id", "abc123456789")
	rec := httptest.NewRecorder()
	app.containerLogsPage(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "first line") || !strings.Contains(body, "data-log-auto-refresh") || strings.Contains(body, "container-command") {
		t.Fatalf("expected log page with refresh controls, code=%d body=%s", rec.Code, body)
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/containers/web/logs/raw", nil)
	req.SetPathValue("id", "web")
	rec = httptest.NewRecorder()
	app.containerLogsRaw(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "first line\nsecond line" {
		t.Fatalf("expected raw logs, code=%d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.Join(calls, "\n"), "docker logs --tail 300 abc123456789") {
		t.Fatalf("expected docker logs call, calls=%v", calls)
	}
}

func TestContainerLogsRejectsPartialIDAndReportsEmptyFailure(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/web","Image":"sha256:image","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{}}}]`, nil
		case "docker logs --tail 300 abc123456789":
			return "", errors.New("logs failed")
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/containers/abc/logs/raw", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()
	app.containerLogsRaw(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected partial container ID to be rejected, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/containers/abc123456789/logs/raw", nil)
	req.SetPathValue("id", "abc123456789")
	rec = httptest.NewRecorder()
	app.containerLogsRaw(rec, req)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "logs unavailable") {
		t.Fatalf("expected fallback log error, code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestContainerExecPage(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/web","Image":"sha256:image","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{}}}]`, nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/containers/abc123456789/exec", nil)
	req.SetPathValue("id", "abc123456789")
	rec := httptest.NewRecorder()
	app.containerExecPage(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "container command") || !strings.Contains(body, "container-command") || !strings.Contains(body, "/containers/abc123456789/logs") {
		t.Fatalf("expected command page, code=%d body=%s", rec.Code, body)
	}
}

func TestContainerExecRunsCommand(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/web","Image":"sha256:image","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{}}}]`, nil
		case "docker exec abc123456789 sh -lc whoami":
			return "root\n", nil
		}
		return "", nil
	}

	req := formRequest("http://docklight.local/containers/abc123456789/exec", url.Values{"command": {"whoami"}})
	req.SetPathValue("id", "abc123456789")
	rec := httptest.NewRecorder()
	app.containerExec(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "root") || !strings.Contains(body, "Command completed") {
		t.Fatalf("expected command output, code=%d body=%s", rec.Code, body)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "docker exec abc123456789 sh -lc whoami") {
		t.Fatalf("expected docker exec call, calls=%v", calls)
	}
	var activityOutput string
	if err := db.QueryRow(`SELECT output FROM activity_log WHERE action = 'container exec'`).Scan(&activityOutput); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(activityOutput, "root") || !strings.Contains(activityOutput, "output bytes: 5") {
		t.Fatalf("expected activity to store output metadata only, got %q", activityOutput)
	}
}

func TestContainerExecValidationAndFailure(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	state := "exited"
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/web","Image":"sha256:image","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"` + state + `","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{}}}]`, nil
		case "docker logs --tail 300 abc123456789":
			return "ready", nil
		case "docker exec abc123456789 sh -lc false":
			return "boom", errors.New("exec failed")
		}
		return "", nil
	}

	req := formRequest("http://docklight.local/containers/abc123456789/exec", url.Values{"command": {"   "}})
	req.SetPathValue("id", "abc123456789")
	rec := httptest.NewRecorder()
	app.containerExec(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "command is required") {
		t.Fatalf("expected blank command validation, code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(strings.Join(calls, "\n"), "docker logs") || strings.Contains(strings.Join(calls, "\n"), "docker exec") {
		t.Fatalf("blank command should not fetch logs or exec, calls=%v", calls)
	}

	calls = nil
	req = formRequest("http://docklight.local/containers/abc123456789/exec", url.Values{"command": {"whoami"}})
	req.SetPathValue("id", "abc123456789")
	rec = httptest.NewRecorder()
	app.containerExec(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "container is not running") {
		t.Fatalf("expected stopped container validation, code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(strings.Join(calls, "\n"), "docker logs") || strings.Contains(strings.Join(calls, "\n"), "docker exec") {
		t.Fatalf("stopped container should not fetch logs or exec, calls=%v", calls)
	}

	calls = nil
	state = "running"
	req = formRequest("http://docklight.local/containers/abc123456789/exec", url.Values{"command": {"false"}})
	req.SetPathValue("id", "abc123456789")
	rec = httptest.NewRecorder()
	app.containerExec(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "boom") || !strings.Contains(body, "Command failed") {
		t.Fatalf("expected failed command output, code=%d body=%s", rec.Code, body)
	}
}

func TestContainerExecRejectsProtectedContainer(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/docklight","Image":"sha256:image","Config":{"Image":"docklight","Labels":{"com.docker.compose.project":"docklight","com.docker.compose.service":"docklight"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{}}}]`, nil
		}
		return "", nil
	}

	req := formRequest("http://docklight.local/containers/abc123456789/exec", url.Values{"command": {"whoami"}})
	req.SetPathValue("id", "abc123456789")
	rec := httptest.NewRecorder()
	app.containerExec(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Docklight cannot run commands against its own container") {
		t.Fatalf("expected protected container validation, code=%d body=%s", rec.Code, body)
	}
	if strings.Contains(strings.Join(calls, "\n"), "docker logs") || strings.Contains(strings.Join(calls, "\n"), "docker exec") {
		t.Fatalf("protected container should not fetch logs or exec, calls=%v", calls)
	}
}

func TestProtectedStackActionsAreBlocked(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "docklight"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('docklight', 'docklight', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		t.Fatal("protected stack action should not run docker")
		return "", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/docklight/stop", nil)
	req.SetPathValue("name", "docklight")
	req.SetPathValue("action", "stop")
	rec := httptest.NewRecorder()
	app.stackAction(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected protected action redirect, got %d", rec.Code)
	}
}

func TestStackActionRecreateAndUpgradeFailureBranches(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := strings.Join(args, " ")
		if strings.Contains(call, "up -d --force-recreate") || strings.Contains(call, "up -d") {
			return "up failed", errors.New("up failed")
		}
		return "ok", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/recreate", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "recreate")
	rec := httptest.NewRecorder()
	app.stackAction(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected recreate up failure redirect, got %d", rec.Code)
	}
	waitForActivityStatus(t, app, "web", "recreate", "failed")

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/upgrade", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "upgrade")
	rec = httptest.NewRecorder()
	app.stackAction(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected upgrade up failure redirect, got %d", rec.Code)
	}
	waitForActivityStatus(t, app, "web", "upgrade", "failed")
	if !protectedStackAction("start") || !protectedStackAction("down") {
		t.Fatal("unexpected protected stack action classification")
	}
}

func TestStackActionMapsAndPersistsCriticalOperations(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}

	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "docker container ls -aq" {
			return "", nil
		}
		return "ok", nil
	}

	for _, action := range []string{"start", "stop", "restart", "rebuild", "down", "upgrade", "upgrade-recreate"} {
		req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/"+action, nil)
		req.SetPathValue("name", "web")
		req.SetPathValue("action", action)
		rec := httptest.NewRecorder()
		app.stackAction(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("%s expected redirect, got %d", action, rec.Code)
		}
		waitForActivityStatus(t, app, "web", action, "success")
	}

	joined := strings.Join(calls, "\n")
	for _, want := range []string{" up -d", " stop", " restart", " up -d --build", " down", " pull", " up -d --force-recreate"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in calls:\n%s", want, joined)
		}
	}
}

func TestStackActionUpgradeStopsAfterPullFailure(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "docker container ls -aq" {
			return "", nil
		}
		if strings.Contains(call, " pull") {
			return "pull failed", errors.New("pull failed")
		}
		return "should not run", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/upgrade", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "upgrade")
	rec := httptest.NewRecorder()
	app.stackAction(rec, req)
	waitForActivityStatus(t, app, "web", "upgrade", "failed")
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, " up -d") {
		t.Fatalf("up should not run after pull failure:\n%s", joined)
	}
}

func TestStackActionUpgradeRecreateStopsAfterPullFailure(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "docker container ls -aq" {
			return "", nil
		}
		if strings.Contains(call, " pull") {
			return "pull failed", errors.New("pull failed")
		}
		return "should not run", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/upgrade-recreate", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "upgrade-recreate")
	rec := httptest.NewRecorder()
	app.stackAction(rec, req)
	waitForActivityStatus(t, app, "web", "upgrade-recreate", "failed")
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, " up -d --force-recreate") {
		t.Fatalf("force recreate should not run after pull failure:\n%s", joined)
	}
}

func TestStackActionUpgradeRecreateRunsDownBeforeUp(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "docker container ls -aq" {
			return "", nil
		}
		return "ok", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/upgrade-recreate", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "upgrade-recreate")
	rec := httptest.NewRecorder()
	app.stackAction(rec, req)
	waitForActivityStatus(t, app, "web", "upgrade-recreate", "success")
	joined := strings.Join(calls, "\n")
	pullIdx := strings.Index(joined, " pull")
	downIdx := strings.Index(joined, " down --remove-orphans")
	upIdx := strings.Index(joined, " up -d --force-recreate")
	if pullIdx < 0 || downIdx < 0 || upIdx < 0 || !(pullIdx < downIdx && downIdx < upIdx) {
		t.Fatalf("expected pull, down, then force recreate; calls:\n%s", joined)
	}
}

func TestStackActionRecreateUnknownAndEditValidation(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "docker container ls -aq" {
			return "", nil
		}
		return "ok", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/recreate", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "recreate")
	rec := httptest.NewRecorder()
	app.stackAction(rec, req)
	waitForActivityStatus(t, app, "web", "recreate", "success")
	if rec.Code != http.StatusFound || !strings.Contains(strings.Join(calls, "\n"), "--force-recreate") {
		t.Fatalf("expected recreate to force recreate, code=%d calls=%v", rec.Code, calls)
	}

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/nope", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "nope")
	rec = httptest.NewRecorder()
	app.stackAction(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected unknown action redirect, got %d", rec.Code)
	}

	values := url.Values{"compose_yaml": {""}}
	req = formRequest("http://docklight.local/stacks/web/edit", values)
	req.SetPathValue("name", "web")
	rec = httptest.NewRecorder()
	app.updateStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "compose yaml is required") {
		t.Fatalf("expected update validation error, code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStackActionRunsAsyncAndRejectsConcurrentOperation(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if strings.Contains(call, " up -d") {
			close(started)
			<-release
		}
		return "ok", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/start", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "start")
	rec := httptest.NewRecorder()
	app.stackAction(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected start redirect, got %d", rec.Code)
	}
	<-started

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/stop", nil)
	req.SetPathValue("name", "web")
	req.SetPathValue("action", "stop")
	rec = httptest.NewRecorder()
	app.stackAction(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected concurrent operation redirect, got %d", rec.Code)
	}
	if key, message, ok := app.readFlashCookie(requestWithCookie(req, rec.Result().Cookies())); !ok || key != "error" || !strings.Contains(message, "another operation is already running") {
		t.Fatalf("expected busy flash, key=%q ok=%v message=%q", key, ok, message)
	}

	close(release)
	waitForActivityStatus(t, app, "web", "start", "success")
}
