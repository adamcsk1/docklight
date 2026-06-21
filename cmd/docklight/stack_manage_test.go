package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEditStackPageRedirectsWhenComposeFileMissing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/stacks/web/edit", nil)
	req.SetPathValue("name", "web")
	rec := httptest.NewRecorder()
	app.editStackPage(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected missing compose redirect, got %d", rec.Code)
	}
}

func TestRescanStacksIsTransactional(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "valid"))
	mustWriteCompose(t, filepath.Join(root, "my stack"))
	mustWriteCompose(t, filepath.Join(root, "my-stack"))

	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('existing', 'existing', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}

	err := app.rescanStacks()
	if err == nil {
		t.Fatal("expected collision error")
	}
	for _, name := range []string{"existing", "valid"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = ?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if name == "existing" && count != 1 {
			t.Fatalf("expected existing row to remain, got %d", count)
		}
		if name == "valid" && count != 0 {
			t.Fatalf("expected valid row to roll back, got %d", count)
		}
	}
}

func TestCreateStackDockerStartFailureRedirectsToStackDetail(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "pull failed", errors.New("start failed")
	}

	values := url.Values{}
	values.Set("name", "failed-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/failed-stack" {
		t.Fatalf("expected redirect to stack detail after start failure, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestEditStackSaveDoesNotRecreateStack(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "editable")
	mustWriteCompose(t, stackDir)
	if err := os.MkdirAll(filepath.Join(stackDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "data", "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('editable', 'editable', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		if strings.Contains(name+" "+strings.Join(args, " "), "docker compose") {
			t.Fatalf("save-only edit should not run compose commands")
		}
		return "ok", nil
	}

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:1.28\n    volumes:\n      - /etc/localtime:/etc/localtime:ro")
	values.Set("bind_folders", "config\ndata")
	values.Set("recreate_folders", "on")
	values.Set("external_bind_mounts_allowed", "on")
	values.Set("update_mode", "save")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/editable" {
		t.Fatalf("expected redirect to stack detail, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	composeBytes, err := os.ReadFile(filepath.Join(stackDir, "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(composeBytes), "nginx:1.28") {
		t.Fatalf("compose file was not updated: %s", string(composeBytes))
	}
	var externalBinds int
	if err := db.QueryRow(`SELECT external_bind_mounts_allowed FROM stacks WHERE name = 'editable'`).Scan(&externalBinds); err != nil {
		t.Fatal(err)
	}
	if externalBinds != 1 {
		t.Fatalf("expected external bind setting to be saved, got %d", externalBinds)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "config")); err != nil {
		t.Fatalf("expected new bind folder to be created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data", "keep.txt")); err != nil {
		t.Fatalf("expected save-only to preserve existing folder contents: %v", err)
	}
}

func TestEditStackRecreateFoldersRemovesListedFolderContents(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "editable")
	mustWriteCompose(t, stackDir)
	if err := os.MkdirAll(filepath.Join(stackDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "data", "old.txt"), []byte("remove"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('editable', 'editable', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:1.27")
	values.Set("bind_folders", "data")
	values.Set("recreate_folders", "on")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data")); err != nil {
		t.Fatalf("expected recreated folder to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data", "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected old file to be removed when recreate is true, stat error: %v", err)
	}
}

func TestStackEnvFilePersistence(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx:${TAG}", "TAG=1.27", nil); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "web", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "TAG=1.27\n" {
		t.Fatalf("unexpected env file content %q", string(content))
	}
	stack := Stack{Name: "web", AbsolutePath: filepath.Join(root, "web"), ComposeFile: "docker-compose.yml"}
	if err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx", "", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "web", ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected empty env editor to remove .env, got %v", err)
	}
}

func TestCreateStackValidationAndFileHelpers(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	installInventoryRunner(app)

	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{name: "bad name", form: url.Values{"name": {"Bad Name"}, "compose_yaml": {"services:\n  app:\n    image: nginx"}}, want: "suggested"},
		{name: "missing compose", form: url.Values{"name": {"web"}}, want: "compose yaml is required"},
		{name: "unsafe compose", form: url.Values{"name": {"web"}, "compose_yaml": {"services:\n  app:\n    privileged: true"}}, want: "blocked host-level option"},
		{name: "bad bind", form: url.Values{"name": {"web"}, "compose_yaml": {"services:\n  app:\n    image: nginx"}, "bind_folders": {"../data"}}, want: "bind folder must be relative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := formRequest("http://docklight.local/stacks/new", tc.form)
			rec := httptest.NewRecorder()
			app.createStack(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("expected create validation %q, code=%d body=%s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}

	if err := app.writeStackFiles("files", "services:\n  app:\n    image: nginx", "", []string{"data", "config/logs"}); err != nil {
		t.Fatal(err)
	}
	stack := Stack{Name: "files", AbsolutePath: filepath.Join(root, "files"), ComposeFile: "docker-compose.yml"}
	if err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx:1.27", "", []string{"data"}, true); err != nil {
		t.Fatal(err)
	}
	if err := ensurePathInside(filepath.Join(root, "files"), filepath.Join(root, "files", "docker-compose.yml")); err != nil {
		t.Fatal(err)
	}
	if err := ensurePathInside(filepath.Join(root, "files"), filepath.Join(root, "other.yml")); err == nil {
		t.Fatal("expected outside path to fail")
	}
}

func TestStackAndSystemActionFailureBranches(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "docker container ls -aq" {
			return "", nil
		}
		return "docker failed", errTestBackend
	}

	for _, action := range []string{"start", "recreate", "upgrade"} {
		t.Run(action, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/"+action, nil)
			req.SetPathValue("name", "web")
			req.SetPathValue("action", action)
			rec := httptest.NewRecorder()
			app.stackAction(rec, req)
			if rec.Code != http.StatusFound {
				t.Fatalf("expected redirect for failed %s, got %d", action, rec.Code)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/missing/start", nil)
	req.SetPathValue("name", "missing")
	req.SetPathValue("action", "start")
	rec := httptest.NewRecorder()
	app.stackAction(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected missing stack redirect, got %d", rec.Code)
	}

	failingRoot := filepath.Join(t.TempDir(), "missing")
	failingApp, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: failingRoot, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/rescan", nil)
	rec = httptest.NewRecorder()
	failingApp.rescan(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected failed rescan redirect, got %d", rec.Code)
	}
}

func TestStackDiscoveryAndLiveEnrichmentBranches(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("services: {}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := findComposeFile(root); got != "docker-compose.yml" {
		t.Fatalf("expected docker-compose.yml precedence, got %q", got)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if got := findComposeFile(missing); got != "" {
		t.Fatalf("expected missing compose file to return empty, got %q", got)
	}

	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	stack := Stack{Name: "web", AbsolutePath: filepath.Join(root, "web"), ComposeFile: "compose.yml"}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "", errors.New("compose failed")
	}
	app.enrichLive(&stack)
	if stack.Status != "unknown" {
		t.Fatalf("expected compose failure to leave status unknown, got %q", stack.Status)
	}

	stack = Stack{Name: "web", AbsolutePath: filepath.Join(root, "web"), ComposeFile: "compose.yml"}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "", nil
	}
	app.enrichLive(&stack)
	if stack.Status != "stopped" || stack.ContainerCount != 0 {
		t.Fatalf("expected empty compose output to be stopped, got %#v", stack)
	}

	stack = Stack{Name: "web", AbsolutePath: filepath.Join(root, "web"), ComposeFile: "compose.yml"}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return `[
{"Name":"docklight","Service":"docklight","State":"running","Status":"Up","Image":"docklight:local"},
{"Name":"api","Service":"api","State":"running","Status":"Up","Image":"nginx"}
]`, nil
	}
	app.enrichLive(&stack)
	if stack.Status != "running" || !stack.Protected || stack.ContainerCount != 2 {
		t.Fatalf("expected running protected stack, got %#v", stack)
	}
}

func TestCreateStackExistingFolderPreservesForm(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	req := formRequest("http://docklight.local/stacks/new", mapValues("name", "web", "compose_yaml", "services:\n  app:\n    image: nginx"))
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "stack folder already exists") || !strings.Contains(rec.Body.String(), "nginx") {
		t.Fatalf("expected existing folder validation with preserved form, code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEditStackUpdatesComposeCreatesFoldersAndRecreatesStack(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "editable")
	mustWriteCompose(t, stackDir)
	if err := os.WriteFile(filepath.Join(stackDir, "data.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('editable', 'editable', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "ok", nil
	}

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:1.27")
	values.Set("bind_folders", "data\nconfig")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/editable" {
		t.Fatalf("expected redirect to stack detail, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	composeBytes, err := os.ReadFile(filepath.Join(stackDir, "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(composeBytes), "nginx:1.27") {
		t.Fatalf("compose file was not updated: %s", string(composeBytes))
	}
	if _, err := os.Stat(filepath.Join(stackDir, "config")); err != nil {
		t.Fatalf("expected new bind folder to be created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data.txt")); err != nil {
		t.Fatalf("expected existing files to remain when recreate is false: %v", err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "down --rmi all --remove-orphans") || !strings.Contains(joined, "up -d --force-recreate") {
		t.Fatalf("expected down --rmi all then force recreate, calls:\n%s", joined)
	}
}

func TestUpdateStackAndDeleteStackFailurePaths(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	mustWriteCompose(t, filepath.Join(root, "docklight"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml'), ('docklight', 'docklight', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "docker container ls -aq" {
			return "", nil
		}
		if strings.Contains(call, " down") {
			return "down failed", errTestBackend
		}
		return "ok", nil
	}

	for _, tc := range []struct {
		name     string
		path     string
		form     url.Values
		want     string
		redirect bool
	}{
		{name: "missing", path: "missing", form: url.Values{"compose_yaml": {"services:\n  app:\n    image: nginx"}}, redirect: true},
		{name: "protected", path: "docklight", form: url.Values{"compose_yaml": {"services:\n  app:\n    image: nginx"}}, redirect: true},
		{name: "unsafe compose", path: "web", form: url.Values{"compose_yaml": {"services:\n  app:\n    network_mode: host"}}, want: "blocked host-level option"},
		{name: "unsafe bind", path: "web", form: url.Values{"compose_yaml": {"services:\n  app:\n    image: nginx"}, "bind_folders": {"/host"}}, want: "bind folder"},
	} {
		t.Run("update "+tc.name, func(t *testing.T) {
			req := formRequest("http://docklight.local/stacks/"+tc.path+"/edit", tc.form)
			req.SetPathValue("name", tc.path)
			rec := httptest.NewRecorder()
			app.updateStack(rec, req)
			if tc.redirect {
				if rec.Code != http.StatusFound {
					t.Fatalf("expected redirect, got %d", rec.Code)
				}
				return
			}
			if !strings.Contains(rec.Body.String()+rec.Header().Get("Set-Cookie"), tc.want) {
				t.Fatalf("expected %q, code=%d body=%s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}

	req := formRequest("http://docklight.local/stacks/web/edit", url.Values{"compose_yaml": {"services:\n  app:\n    image: nginx:1.28"}})
	req.SetPathValue("name", "web")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected down failure redirect, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/missing/delete", nil)
	req.SetPathValue("name", "missing")
	rec = httptest.NewRecorder()
	app.deleteStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected missing delete redirect, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/docklight/delete", nil)
	req.SetPathValue("name", "docklight")
	rec = httptest.NewRecorder()
	app.deleteStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected protected delete redirect, got %d", rec.Code)
	}
}
