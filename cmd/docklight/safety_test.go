package main

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestValidateStackName(t *testing.T) {
	good, err := validateStackName("my-stack_1")
	if err != nil || good != "my-stack_1" {
		t.Fatalf("expected valid stack name, got %q, %v", good, err)
	}

	for _, value := range []string{"", "MyStack", "../x", "my stack", "x/y"} {
		if _, err := validateStackName(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestParseBindFoldersStaysRelative(t *testing.T) {
	folders, err := parseBindFolders("data\nconfig/logs\ndata\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(folders, ",") != "data,config/logs" {
		t.Fatalf("unexpected folders: %#v", folders)
	}

	for _, value := range []string{"../data", "/data", `C:\\data`, `data\\..\\x`} {
		if _, err := parseBindFolders(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestEnsureStackPathSafe(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root}}
	if err := app.ensureStackPathSafe(filepath.Join(root, "stack", "data")); err != nil {
		t.Fatalf("expected child path to be safe: %v", err)
	}
	if err := app.ensureStackPathSafe(root); err == nil {
		t.Fatal("expected root path to be rejected")
	}
	if err := app.ensureStackPathSafe(filepath.Dir(root)); err == nil {
		t.Fatal("expected parent path to be rejected")
	}
}

func TestRescanRejectsStackNameCollision(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "my stack"))
	mustWriteCompose(t, filepath.Join(root, "my-stack"))

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	app := &App{cfg: Config{StacksRoot: root}, db: db}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	err = app.rescanStacks()
	if err == nil || !strings.Contains(err.Error(), "both map") {
		t.Fatalf("expected collision error, got %v", err)
	}
}

func TestRebuildDatabaseDropsExistingDataOnRescanFailure(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "my stack"))
	mustWriteCompose(t, filepath.Join(root, "my-stack"))

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	app := &App{cfg: Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret"}, db: db}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('existing', 'existing', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/database/rebuild", nil)
	rec := httptest.NewRecorder()
	app.rebuildDatabase(rec, req)

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'existing'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected existing stack row to be dropped by rebuild, got count %d", count)
	}
}

func TestRebuildDatabaseStartsFreshFromFilesystem(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	app := &App{cfg: Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret"}, db: db}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO stacks (name, relative_path, compose_file, external_bind_mounts_allowed) VALUES ('web', 'old-web', 'compose.yml', 1);
INSERT INTO activity_log (stack_name, action, status, output) VALUES ('old', 'test', 'success', 'old row');
INSERT INTO app_settings (key, value) VALUES ('verbose_logging_enabled', 'true') ON CONFLICT(key) DO UPDATE SET value = excluded.value;
INSERT INTO sessions (id_hash, username, expires_at) VALUES ('old-session', 'admin', '2099-01-01T00:00:00Z');
`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/database/rebuild", nil)
	rec := httptest.NewRecorder()
	app.rebuildDatabase(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected rebuild redirect, got %d", rec.Code)
	}

	var relativePath string
	var externalBindsAllowed int
	if err := db.QueryRow(`SELECT relative_path, external_bind_mounts_allowed FROM stacks WHERE name = 'web'`).Scan(&relativePath, &externalBindsAllowed); err != nil {
		t.Fatal(err)
	}
	if relativePath != "web" || externalBindsAllowed != 0 {
		t.Fatalf("expected fresh filesystem stack row, path=%q external_binds=%d", relativePath, externalBindsAllowed)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM activity_log WHERE stack_name = 'old'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected old activity rows to be dropped, got count %d", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id_hash = 'old-session'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected old sessions to be dropped, got count %d", count)
	}
	var verboseLogging string
	if err := db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, settingVerboseLoggingEnabled).Scan(&verboseLogging); err != nil {
		t.Fatal(err)
	}
	if verboseLogging != "false" {
		t.Fatalf("expected default verbose logging after rebuild, got %q", verboseLogging)
	}
}

func TestRescanResetsExternalBindPermissionForResurrectedStack(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := &App{cfg: Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret"}, db: db}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file, ignored, external_bind_mounts_allowed) VALUES ('web', 'old-web', 'compose.yml', 1, 1)`); err != nil {
		t.Fatal(err)
	}

	if err := app.rescanStacks(); err != nil {
		t.Fatal(err)
	}
	var allowed int
	if err := db.QueryRow(`SELECT external_bind_mounts_allowed FROM stacks WHERE name = 'web'`).Scan(&allowed); err != nil {
		t.Fatal(err)
	}
	if allowed != 0 {
		t.Fatalf("expected resurrected stack to reset external bind permission, got %d", allowed)
	}
}

func TestCreateStackPostWriteErrorPreservesFormValues(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "my stack"))
	mustWriteCompose(t, filepath.Join(root, "my-stack"))

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	app := &App{cfg: Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret"}, db: db}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}

	values := url.Values{}
	values.Set("name", "new-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest")
	values.Set("bind_folders", "data\nconfig")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	app.createStack(rec, req)
	body := rec.Body.String()
	for _, expected := range []string{"new-stack", "nginx:latest", "data", "config", "stack files were created, but scan failed"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected response to preserve %q, body: %s", expected, body)
		}
	}
}

func TestValidRequestOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/x/start", nil)
	req.Header.Set("Origin", "http://docklight.local")
	if !validRequestOrigin(req) {
		t.Fatal("expected matching origin to be accepted")
	}

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/x/start", nil)
	req.Header.Set("Origin", "http://evil.local")
	if validRequestOrigin(req) {
		t.Fatal("expected cross-site origin to be rejected")
	}

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/x/start", nil)
	if validRequestOrigin(req) {
		t.Fatal("expected missing origin and referer to be rejected")
	}
}

func TestFlashRedirectDoesNotPutMessageInURL(t *testing.T) {
	app := &App{cfg: Config{BaseURL: "/", SessionSecret: "secret"}}
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/action", nil)
	req.Header.Set("Referer", "http://docklight.local/stacks/x?error=old")
	rec := httptest.NewRecorder()

	app.redirectWithError(rec, req, "/", "docker output with secret")
	location := rec.Header().Get("Location")
	if strings.Contains(location, "docker") || strings.Contains(location, "secret") || strings.Contains(location, "error=") {
		t.Fatalf("flash leaked into redirect URL: %s", location)
	}
	if rec.Result().Cookies()[0].Name != "docklight_flash" {
		t.Fatal("expected flash cookie")
	}
}

func TestLimitedOutputCapsCapturedBytes(t *testing.T) {
	output := &limitedOutput{limit: 5}
	n, err := output.Write([]byte("hello world"))
	if err != nil || n != len("hello world") {
		t.Fatalf("unexpected write result: %d, %v", n, err)
	}
	if got := output.String(); got != "hello\n... output truncated" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, value := range []string{"/", "/docklight", "/nested/path_1"} {
		if err := validateBaseURL(value); err != nil {
			t.Fatalf("expected %q to be valid: %v", value, err)
		}
	}
	for _, value := range []string{"docklight", "/../x", "/bad?x", "/bad%2fpath", "/bad.path"} {
		if err := validateBaseURL(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestValidateComposeYAMLBlocksHostLevelOptions(t *testing.T) {
	if err := validateComposeYAML("services:\n  app:\n    image: nginx\n"); err != nil {
		t.Fatalf("expected basic compose to pass: %v", err)
	}
	for _, value := range []string{"privileged: true", "/var/run/docker.sock", "network_mode: host", "cap_add:"} {
		if err := validateComposeYAML(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestDeleteStackRemovesComposeVolumesImagesFilesAndDatabaseRow(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "victim"))
	app, db := newDeleteStackTestApp(t, root)

	var calls []string
	inspectCalls := 0
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "docker container ls -aq" {
			if inspectCalls == 0 {
				return "victim-container\nother-container\n", nil
			}
			return "other-container\n", nil
		}
		if strings.HasPrefix(call, "docker container inspect") {
			inspectCalls++
			if inspectCalls == 1 {
				return `[
                    {"ID":"victim-container","Name":"/victim-app","Image":"sha256:victimimage","Config":{"Image":"app:victim","Labels":{"com.docker.compose.project":"victim","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}},
                    {"ID":"other-container","Name":"/other-app","Image":"sha256:otherimage","Config":{"Image":"app:other","Labels":{"com.docker.compose.project":"other","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}
                ]`, nil
			}
			return `[
                {"ID":"other-container","Name":"/other-app","Image":"sha256:otherimage","Config":{"Image":"app:other","Labels":{"com.docker.compose.project":"other","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}
            ]`, nil
		}
		if strings.Contains(call, "docker compose") && strings.Contains(call, " down --remove-orphans --volumes") {
			return "stack removed", nil
		}
		if call == "docker image rm sha256:victimimage" {
			return "image removed", nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/victim/delete", nil)
	req.SetPathValue("name", "victim")
	rec := httptest.NewRecorder()
	app.deleteStack(rec, req)

	if _, err := os.Stat(filepath.Join(root, "victim")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stack folder to be deleted, stat error: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'victim'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected stack row to be deleted, got count %d", count)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "docker compose") || !strings.Contains(joined, " down --remove-orphans --volumes") {
		t.Fatalf("expected compose down with volumes, calls:\n%s", joined)
	}
	if !strings.Contains(joined, "docker image rm sha256:victimimage") {
		t.Fatalf("expected unused stack image to be removed, calls:\n%s", joined)
	}
}

func TestDeleteStackSuccessRedirectsToDashboardWithFlash(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "victim"))
	app, _ := newDeleteStackTestApp(t, root)

	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "docker container ls -aq" {
			return "", nil
		}
		if strings.Contains(call, "docker compose") && strings.Contains(call, " down --remove-orphans --volumes") {
			return "stack removed", nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/victim/delete", nil)
	req.Header.Set("Referer", "http://docklight.local/")
	req.SetPathValue("name", "victim")
	rec := httptest.NewRecorder()
	app.deleteStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("expected redirect to dashboard, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "docklight_flash") {
		t.Fatalf("expected success flash cookie after delete, got %q", rec.Header().Get("Set-Cookie"))
	}
	flashReq := httptest.NewRequest(http.MethodGet, "http://docklight.local/", nil)
	for _, cookie := range rec.Result().Cookies() {
		flashReq.AddCookie(cookie)
	}
	key, message, ok := app.readFlashCookie(flashReq)
	if !ok || key != "message" || message != "stack deleted: victim" {
		t.Fatalf("expected delete success flash, key=%q message=%q ok=%v", key, message, ok)
	}
}

func TestDeleteStackSkipsImageStillUsedByAnotherContainer(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "victim"))
	app, _ := newDeleteStackTestApp(t, root)

	var calls []string
	inspectCalls := 0
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "docker container ls -aq" {
			if inspectCalls == 0 {
				return "victim-container\nother-container\n", nil
			}
			return "other-container\n", nil
		}
		if strings.HasPrefix(call, "docker container inspect") {
			inspectCalls++
			if inspectCalls == 1 {
				return `[
                    {"ID":"victim-container","Name":"/victim-app","Image":"sha256:sharedimage","Config":{"Image":"app:shared","Labels":{"com.docker.compose.project":"victim","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}},
                    {"ID":"other-container","Name":"/other-app","Image":"sha256:sharedimage","Config":{"Image":"app:shared","Labels":{"com.docker.compose.project":"other","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}
                ]`, nil
			}
			return `[
                {"ID":"other-container","Name":"/other-app","Image":"sha256:sharedimage","Config":{"Image":"app:shared","Labels":{"com.docker.compose.project":"other","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}
            ]`, nil
		}
		if strings.Contains(call, "docker compose") && strings.Contains(call, " down --remove-orphans --volumes") {
			return "stack removed", nil
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/victim/delete", nil)
	req.SetPathValue("name", "victim")
	rec := httptest.NewRecorder()
	app.deleteStack(rec, req)

	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "docker image rm") {
		t.Fatalf("expected shared image to be preserved, calls:\n%s", joined)
	}
}

func TestDeleteStackPreservesFilesAndDatabaseRowWhenDockerCleanupFails(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "victim"))
	app, db := newDeleteStackTestApp(t, root)

	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "docker container ls -aq" {
			return "victim-container\n", nil
		}
		if strings.HasPrefix(call, "docker container inspect") {
			return `[
                {"ID":"victim-container","Name":"/victim-app","Image":"sha256:victimimage","Config":{"Image":"app:victim","Labels":{"com.docker.compose.project":"victim","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}
            ]`, nil
		}
		if strings.Contains(call, "docker compose") && strings.Contains(call, " down --remove-orphans --volumes") {
			return "compose failed", errors.New("compose failed")
		}
		return "", nil
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/victim/delete", nil)
	req.SetPathValue("name", "victim")
	rec := httptest.NewRecorder()
	app.deleteStack(rec, req)

	if _, err := os.Stat(filepath.Join(root, "victim")); err != nil {
		t.Fatalf("expected stack folder to remain after Docker failure: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'victim'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected stack row to remain after Docker failure, got count %d", count)
	}
}

func newDeleteStackTestApp(t *testing.T, root string) (*App, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret"}, db: db}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('victim', 'victim', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return app, db
}

func mustWriteCompose(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services:\n  app:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
