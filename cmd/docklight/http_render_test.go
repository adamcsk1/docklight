package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestPageHandlersRenderOperationalInventories(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.logActivity("web", "test", "success", "hello")
	installInventoryRunner(app)

	for _, tc := range []struct {
		name    string
		method  func(http.ResponseWriter, *http.Request)
		url     string
		pathKey string
		pathVal string
		want    string
	}{
		{name: "new stack", method: app.newStackPage, url: "http://docklight.local/stacks/new", want: "New stack"},
		{name: "edit stack", method: app.editStackPage, url: "http://docklight.local/stacks/web/edit", pathKey: "name", pathVal: "web", want: "Edit web"},
		{name: "activity", method: app.activity, url: "http://docklight.local/activity", want: "hello"},
		{name: "settings", method: app.settings, url: "http://docklight.local/settings", want: "Settings"},
		{name: "containers", method: app.containers, url: "http://docklight.local/containers", want: "web"},
		{name: "images", method: app.images, url: "http://docklight.local/images", want: "unused:latest"},
		{name: "volumes", method: app.volumes, url: "http://docklight.local/volumes", want: "unusedvol"},
		{name: "networks", method: app.networks, url: "http://docklight.local/networks", want: "unusednet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if tc.pathKey != "" {
				req.SetPathValue(tc.pathKey, tc.pathVal)
			}
			rec := httptest.NewRecorder()
			tc.method(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("expected %s page to contain %q, code=%d body=%s", tc.name, tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCommandActionsDisableWhenUnavailable(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/containers", nil)
	rec := httptest.NewRecorder()
	app.render(rec, req, "containers.html", PageData{Containers: []DockerContainer{
		{ID: "run123", Name: "web", State: "running"},
		{ID: "stop123", Name: "worker", State: "exited"},
		{ID: "dock123", Name: "docklight", State: "running", Protected: true},
	}})
	body := rec.Body.String()
	if !strings.Contains(body, `/containers/run123/exec`) {
		t.Fatalf("expected running unprotected container command link: %s", body)
	}
	if strings.Contains(body, `/containers/stop123/exec`) || strings.Contains(body, `/containers/dock123/exec`) {
		t.Fatalf("expected unavailable command actions to avoid links: %s", body)
	}
	if strings.Count(body, `aria-disabled="true">Command`) != 2 {
		t.Fatalf("expected two disabled command actions: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/stacks/web", nil)
	rec = httptest.NewRecorder()
	app.render(rec, req, "stack.html", PageData{Stack: Stack{Name: "web", Containers: []Container{
		{Name: "api", State: "running"},
		{Name: "job", State: "exited"},
	}}})
	body = rec.Body.String()
	if !strings.Contains(body, `/containers/api/exec`) || strings.Contains(body, `/containers/job/exec`) || !strings.Contains(body, `aria-disabled="true">Command`) {
		t.Fatalf("expected stack command actions to follow running state: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/stacks/docklight", nil)
	rec = httptest.NewRecorder()
	app.render(rec, req, "stack.html", PageData{Stack: Stack{Name: "docklight", Protected: true, Containers: []Container{{Name: "docklight", State: "running"}}}})
	body = rec.Body.String()
	if strings.Contains(body, `/containers/docklight/exec`) || !strings.Contains(body, `aria-disabled="true">Command`) {
		t.Fatalf("expected protected stack command action to be disabled: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/containers/stop123/logs", nil)
	rec = httptest.NewRecorder()
	app.render(rec, req, "container_logs.html", PageData{Container: DockerContainer{ID: "stop123", Name: "worker", State: "exited"}})
	body = rec.Body.String()
	if strings.Contains(body, `/containers/stop123/exec`) || !strings.Contains(body, `aria-disabled="true">Command`) || !strings.Contains(body, `data-log-copy`) {
		t.Fatalf("expected logs page command action to be disabled: %s", body)
	}
}

func TestRenderAndFlashRejectInvalidInputs(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/docklight", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := app.readFlashCookie(httptest.NewRequest(http.MethodGet, "http://docklight.local/", nil)); ok {
		t.Fatal("expected missing flash cookie to be ignored")
	}
	badFlash := httptest.NewRequest(http.MethodGet, "http://docklight.local/", nil)
	badFlash.AddCookie(&http.Cookie{Name: "docklight_flash", Value: app.signFlash("badkey\nmessage")})
	if _, _, ok := app.readFlashCookie(badFlash); ok {
		t.Fatal("expected unsupported flash key to be ignored")
	}
	if _, ok := app.verifyFlash("*." + app.signFlashBytes("*")); ok {
		t.Fatal("expected invalid flash base64 to be rejected")
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/docklight/action", nil)
	req.Header.Set("Referer", "http://evil.local/steal")
	rec := httptest.NewRecorder()
	app.redirectWithMessage(rec, req, "/settings", "saved")
	if rec.Header().Get("Location") != "/docklight/settings" {
		t.Fatalf("expected external referer fallback, got %q", rec.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/docklight/settings", nil)
	rec = httptest.NewRecorder()
	app.render(rec, req, "missing.html", PageData{Title: "Missing"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected missing template error, got %d", rec.Code)
	}
}

func TestOperationalActionsRenderConfirmationPrompts(t *testing.T) {
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
		if strings.Contains(strings.Join(args, " "), "ps") {
			return "[]", nil
		}
		return "", nil
	}

	for _, tc := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		url     string
		want    []string
	}{
		{name: "dashboard", handler: app.dashboard, url: "http://docklight.local/", want: []string{"web", "compose.yml"}},
		{name: "stack detail", handler: app.stackDetail, url: "http://docklight.local/stacks/web", want: []string{"Start stack web", "Stop stack web", "Restart stack web", "Pull images and apply with docker compose up -d", "This restarts all services and resets container uptime", "Pull + apply", "Pull + force upgrade"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			req.SetPathValue("name", "web")
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			body := rec.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Fatalf("expected confirmation %q in body: %s", want, body)
				}
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/", nil)
	rec := httptest.NewRecorder()
	app.dashboard(rec, req)
	body := rec.Body.String()
	for _, removed := range []string{"Start stack web", "Stop stack web", "Restart stack web", "Pull images and apply with docker compose up -d", "Delete</button>"} {
		if strings.Contains(body, removed) {
			t.Fatalf("dashboard should not render stack action %q: %s", removed, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/containers", nil)
	rec = httptest.NewRecorder()
	app.render(rec, req, "containers.html", PageData{Containers: []DockerContainer{{ID: "abc123", Name: "web"}}})
	body = rec.Body.String()
	if !strings.Contains(body, `Delete selected`) || !strings.Contains(body, `data-confirm-delete="selected containers"`) {
		t.Fatalf("expected container delete controls in body: %s", body)
	}
	for _, removed := range []string{"Start selected containers", "Stop selected containers", "Restart selected containers"} {
		if strings.Contains(body, removed) {
			t.Fatalf("did not expect container action %q in body: %s", removed, body)
		}
	}
}
