package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func managedTestFolders(paths ...string) []StackManagedConfigFolder {
	folders := make([]StackManagedConfigFolder, 0, len(paths))
	for _, path := range paths {
		folders = append(folders, StackManagedConfigFolder{Path: path})
	}
	return folders
}

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
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/stacks/failed-stack?operation=") {
		t.Fatalf("expected redirect to stack detail after start failure, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	waitForActivityStatus(t, app, "failed-stack", "create stack", "failed")
}

func TestRenameStackStopsRenamesUpdatesDBAndStarts(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	var commands []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "container" {
			return "", nil
		}
		commands = append(commands, strings.Join(args, " "))
		return "ok", nil
	}

	req := renameStackRequest("web", "api")
	rec := httptest.NewRecorder()
	app.renameStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/api" {
		t.Fatalf("expected redirect to renamed stack, code=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "web")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected old folder removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "api", "compose.yml")); err != nil {
		t.Fatalf("expected renamed compose file: %v", err)
	}
	var relativePath, composeFile string
	if err := db.QueryRow(`SELECT relative_path, compose_file FROM stacks WHERE name = 'api'`).Scan(&relativePath, &composeFile); err != nil {
		t.Fatal(err)
	}
	if relativePath != "api" || composeFile != "compose.yml" {
		t.Fatalf("unexpected renamed row: relative_path=%q compose_file=%q", relativePath, composeFile)
	}
	if len(commands) != 2 || !strings.Contains(commands[0], "--project-name web") || !strings.HasSuffix(commands[0], " down --remove-orphans") || !strings.Contains(commands[1], "--project-name api") || !strings.HasSuffix(commands[1], " up -d") {
		t.Fatalf("unexpected compose commands: %#v", commands)
	}
}

func TestRenameStackRejectsProtectedDocklight(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "docklight"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('docklight', 'docklight', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}

	req := renameStackRequest("docklight", "new-name")
	rec := httptest.NewRecorder()
	app.renameStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/docklight" {
		t.Fatalf("expected redirect to protected stack, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(root, "docklight", "compose.yml")); err != nil {
		t.Fatalf("expected protected folder unchanged: %v", err)
	}
}

func TestRenameStackRejectsStackWithDocklightContainer(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case command == "container ls -aq":
			return "docklight-id", nil
		case strings.HasPrefix(command, "container inspect"):
			return `[{
				"ID":"docklight-id",
				"Name":"/docklight",
				"Config":{"Image":"docklight","Labels":{"com.docker.compose.project":"web","com.docker.compose.service":"docklight"}},
				"State":{"Status":"running"},
				"NetworkSettings":{"Ports":{},"Networks":{}}
			}]`, nil
		default:
			t.Fatalf("unexpected docker command: %s", command)
			return "", nil
		}
	}

	req := renameStackRequest("web", "api")
	rec := httptest.NewRecorder()
	app.renameStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/web" {
		t.Fatalf("expected redirect to original stack, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(root, "web", "compose.yml")); err != nil {
		t.Fatalf("expected protected stack folder unchanged: %v", err)
	}
}

func TestRenameStackRejectsDestinationCollision(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	mustWriteCompose(t, filepath.Join(root, "api"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml'), ('api', 'api', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "container" {
			return "", nil
		}
		t.Fatal("docker should not run for collision")
		return "", nil
	}

	req := renameStackRequest("web", "api")
	rec := httptest.NewRecorder()
	app.renameStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/web" {
		t.Fatalf("expected redirect to old stack, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(root, "web", "compose.yml")); err != nil {
		t.Fatalf("expected old folder unchanged: %v", err)
	}
}

func TestRenameStackDownFailureLeavesFolderAndDBUnchanged(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "container" {
			return "", nil
		}
		return "down failed", errors.New("down failed")
	}

	req := renameStackRequest("web", "api")
	rec := httptest.NewRecorder()
	app.renameStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/web" {
		t.Fatalf("expected redirect to old stack, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(root, "web", "compose.yml")); err != nil {
		t.Fatalf("expected old folder unchanged: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'web' AND relative_path = 'web'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected old DB row unchanged, count=%d err=%v", count, err)
	}
}

func TestRenameStackUpFailureKeepsRenamedStack(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "container" {
			return "", nil
		}
		if args[len(args)-2] == "up" {
			return "up failed", errors.New("up failed")
		}
		return "down ok", nil
	}

	req := renameStackRequest("web", "api")
	rec := httptest.NewRecorder()
	app.renameStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/api" {
		t.Fatalf("expected redirect to renamed stack after up failure, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(root, "api", "compose.yml")); err != nil {
		t.Fatalf("expected renamed folder to remain: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'api' AND relative_path = 'api'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected renamed DB row, count=%d err=%v", count, err)
	}
}

func TestRenameStackPostDownDBFailureRestartsOriginalStack(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	var commands []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "container" {
			return "", nil
		}
		command := strings.Join(args, " ")
		commands = append(commands, command)
		if strings.HasSuffix(command, " down --remove-orphans") {
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		return "ok", nil
	}

	req := renameStackRequest("web", "api")
	rec := httptest.NewRecorder()
	app.renameStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/web" {
		t.Fatalf("expected redirect to original stack, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(root, "web", "compose.yml")); err != nil {
		t.Fatalf("expected old folder restored: %v", err)
	}
	if len(commands) != 2 || !strings.HasSuffix(commands[0], " down --remove-orphans") || !strings.HasSuffix(commands[1], " up -d") || !strings.Contains(commands[1], "--project-name web") {
		t.Fatalf("expected down followed by original stack restart, commands=%#v", commands)
	}
}

func TestRenameStackReusesIgnoredDestinationRow(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file, ignored) VALUES ('web', 'web', 'compose.yml', 0), ('api', 'api', 'compose.yml', 1)`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "container" {
			return "", nil
		}
		return "ok", nil
	}

	req := renameStackRequest("web", "api")
	rec := httptest.NewRecorder()
	app.renameStack(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/stacks/api" {
		t.Fatalf("expected redirect to renamed stack, code=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'api' AND relative_path = 'api' AND ignored = 0`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one active api row, count=%d err=%v", count, err)
	}
}

func TestRenameStackDBFailureRollsBackFolder(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	stack := Stack{Name: "web", RelativePath: "web", ComposeFile: "compose.yml", AbsolutePath: filepath.Join(root, "web")}
	err := app.renameStackFilesAndRow(stack, "api", filepath.Join(root, "api"))
	if err == nil {
		t.Fatal("expected DB failure")
	}
	if _, err := os.Stat(filepath.Join(root, "web", "compose.yml")); err != nil {
		t.Fatalf("expected old folder restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "api")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected destination folder removed by rollback, err=%v", err)
	}
}

func renameStackRequest(oldName, newName string) *http.Request {
	values := url.Values{}
	values.Set("new_name", newName)
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/"+oldName+"/rename", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", oldName)
	return req
}

func TestCreateStackPersistsDockerSocketMountPermissionInConfig(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("name", "socket-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock")
	values.Set("docker_socket_mounts_allowed", "on")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected create redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}

	managedConfig, err := app.loadManagedStackConfig(filepath.Join(root, "socket-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if managedConfig.Settings.ExternalBindMountsAllowed || !managedConfig.Settings.DockerSocketMountsAllowed || managedConfig.Settings.USBDeviceBindMountsAllowed || managedConfig.Settings.DBusBindMountAllowed || managedConfig.Settings.HostPIDAllowed {
		t.Fatalf("expected only docker socket permission in managed config: %#v", managedConfig.Settings)
	}
}

func TestCreateStackPersistsUSBDeviceBindPermissionInConfig(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("name", "usb-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    volumes:\n      - /dev/bus/usb:/dev/bus/usb")
	values.Set("usb_device_bind_mounts_allowed", "on")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected create redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}

	managedConfig, err := app.loadManagedStackConfig(filepath.Join(root, "usb-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if !managedConfig.Settings.USBDeviceBindMountsAllowed {
		t.Fatalf("expected USB device bind permission in managed config: %#v", managedConfig.Settings)
	}
}

func TestCreateStackPersistsHostPIDPermissionInConfig(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("name", "pid-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    pid: host")
	values.Set("host_pid_allowed", "on")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected create redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}

	managedConfig, err := app.loadManagedStackConfig(filepath.Join(root, "pid-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if !managedConfig.Settings.HostPIDAllowed {
		t.Fatalf("expected host PID permission in managed config: %#v", managedConfig.Settings)
	}
}

func TestCreateStackPersistsPrivilegedPermissionInConfig(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("name", "privileged-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    privileged: true")
	values.Set("privileged_allowed", "on")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected create redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}

	managedConfig, err := app.loadManagedStackConfig(filepath.Join(root, "privileged-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if !managedConfig.Settings.PrivilegedAllowed {
		t.Fatalf("expected privileged permission in managed config: %#v", managedConfig.Settings)
	}
}

func TestCreateStackPersistsSecurityOptPermissionInConfig(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("name", "security-opt-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    security_opt:\n      - no-new-privileges:true")
	values.Set("security_opt_allowed", "on")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected create redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}

	managedConfig, err := app.loadManagedStackConfig(filepath.Join(root, "security-opt-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if !managedConfig.Settings.SecurityOptAllowed {
		t.Fatalf("expected security_opt permission in managed config: %#v", managedConfig.Settings)
	}
}

func TestCreateStackPersistsDBusBindPermissionInConfig(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("name", "dbus-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    volumes:\n      - /run/dbus:/run/dbus")
	values.Set("dbus_bind_mount_allowed", "on")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected create redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}

	managedConfig, err := app.loadManagedStackConfig(filepath.Join(root, "dbus-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if !managedConfig.Settings.DBusBindMountAllowed {
		t.Fatalf("expected DBus bind permission in managed config: %#v", managedConfig.Settings)
	}
}

func TestCreateStackWritesManagedFiles(t *testing.T) {
	root := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("name", "managed-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest")
	values.Add("managed_folder_path", "data")
	values.Add("managed_folder_chmod", "0755")
	values.Add("managed_folder_chown", "")
	values.Add("managed_folder_path", "data/subfolder")
	values.Add("managed_folder_chmod", "")
	values.Add("managed_folder_chown", "")
	values.Add("managed_file_path", "test.conf")
	values.Add("managed_file_content", "root=true\n")
	values.Add("managed_file_chmod", "0640")
	values.Add("managed_file_chown", "")
	values.Add("managed_file_path", "data/file.conf")
	values.Add("managed_file_content", "child=true\n")
	values.Add("managed_file_chmod", "")
	values.Add("managed_file_chown", "")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected create redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}
	stackDir := filepath.Join(root, "managed-stack")
	if content, err := os.ReadFile(filepath.Join(stackDir, "test.conf")); err != nil || string(content) != "root=true\n" {
		t.Fatalf("expected managed root file, content=%q err=%v", string(content), err)
	}
	if content, err := os.ReadFile(filepath.Join(stackDir, "data", "file.conf")); err != nil || string(content) != "child=true\n" {
		t.Fatalf("expected managed nested file, content=%q err=%v", string(content), err)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data", "subfolder")); err != nil {
		t.Fatalf("expected managed nested folder: %v", err)
	}
	managedConfig, err := os.ReadFile(filepath.Join(stackDir, stackManagedConfigFile))
	if err != nil || !strings.Contains(string(managedConfig), "data/file.conf") || !strings.Contains(string(managedConfig), "chmod: \"0755\"") || !strings.Contains(string(managedConfig), "chmod: \"0640\"") {
		t.Fatalf("expected managed config to list data/file.conf, content=%s err=%v", string(managedConfig), err)
	}
}

func TestManagedFolderConfigLoadsObjectForms(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, stackManagedConfigFile), []byte("folders:\n  - path: data\n  - path: config\n    chmod: \"0750\"\n    chown: \"1000:1000\"\nfiles:\n  - path: test.conf\n    chmod: \"0640\"\n    chown: \"1000:1000\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root}}

	managedConfig, err := app.loadManagedStackConfig(stackDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(managedConfig.Folders) != 2 || managedConfig.Folders[0].Path != "data" || managedConfig.Folders[1].Path != "config" || managedConfig.Folders[1].Chmod != "0750" || managedConfig.Folders[1].Chown != "1000:1000" {
		t.Fatalf("unexpected managed folders: %#v", managedConfig.Folders)
	}
	if len(managedConfig.Files) != 1 || managedConfig.Files[0].Path != "test.conf" || managedConfig.Files[0].Chmod != "0640" || managedConfig.Files[0].Chown != "1000:1000" {
		t.Fatalf("unexpected managed files: %#v", managedConfig.Files)
	}
}

func TestManagedFolderConfigRejectsLegacyScalarForms(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, stackManagedConfigFile), []byte("folders:\n  - data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root}}

	if _, err := app.loadManagedStackConfig(stackDir); err == nil {
		t.Fatal("expected legacy scalar folder config to be rejected")
	}
}

func TestManagedFolderValidationRejectsBadChmodAndChown(t *testing.T) {
	for _, folder := range []ManagedFolderForm{{Path: "data", Chmod: "abc"}, {Path: "data", Chown: "user:group"}, {Path: "data", Chown: "1000"}, {Path: "data", Chown: ":1000"}} {
		if _, err := parseManagedFolderForms([]ManagedFolderForm{folder}); err == nil {
			t.Fatalf("expected invalid managed folder to be rejected: %#v", folder)
		}
	}
}

func TestManagedFolderOwnerParsesNumericUIDGID(t *testing.T) {
	uid, gid, err := parseFolderOwner("1000:1001")
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1000 || gid != 1001 {
		t.Fatalf("unexpected owner uid=%d gid=%d", uid, gid)
	}
}

func TestManagedFileValidationRejectsBadChmodAndChown(t *testing.T) {
	for _, file := range []ManagedFileForm{{Path: "test.conf", Chmod: "abc"}, {Path: "test.conf", Chown: "user:group"}, {Path: "test.conf", Chown: "1000"}, {Path: "test.conf", Chown: ":1000"}} {
		if _, err := parseManagedFiles([]ManagedFileForm{file}); err == nil {
			t.Fatalf("expected invalid managed file to be rejected: %#v", file)
		}
	}
}

func TestManagedFileOwnerParsesNumericUIDGID(t *testing.T) {
	uid, gid, err := parseFileOwner("1000:1001")
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1000 || gid != 1001 {
		t.Fatalf("unexpected owner uid=%d gid=%d", uid, gid)
	}
}

func TestManagedFolderChmodApplied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX folder modes")
	}
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", []StackManagedConfigFolder{{Path: "data/subfolder", Chmod: "0700"}}, nil, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "web", "data", "subfolder"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("expected chmod 0700, got %v", info.Mode().Perm())
	}
}

func TestManagedFileChmodApplied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file modes")
	}
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", nil, []managedFileWrite{{Path: "test.conf", Content: "x", Chmod: "0600"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "web", "test.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected chmod 0600, got %v", info.Mode().Perm())
	}
}

func TestManagedEnvFileDefaultChmodCanBeOverridden(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file modes")
	}
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", nil, []managedFileWrite{{Path: ".env", Content: "TAG=1", Chmod: "0644"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "web", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("expected overridden .env chmod 0644, got %v", info.Mode().Perm())
	}
}

func TestStackFileOwnerAppliedOnCreate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is not supported on Windows")
	}
	root := t.TempDir()
	calls := recordChown(t)
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute, StackFileOwner: &fileOwner{UID: 1000, GID: 1001}}}

	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", []StackManagedConfigFolder{{Path: "data/subfolder"}}, []managedFileWrite{{Path: "test.conf", Content: "x"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	assertChownCall(t, *calls, stackDir, 1000, 1001)
	assertChownCall(t, *calls, filepath.Join(stackDir, "docker-compose.yml"), 1000, 1001)
	assertChownCall(t, *calls, filepath.Join(stackDir, stackManagedConfigFile), 1000, 1001)
	assertChownCall(t, *calls, filepath.Join(stackDir, "test.conf"), 1000, 1001)
	assertChownCall(t, *calls, filepath.Join(stackDir, "data"), 1000, 1001)
	assertChownCall(t, *calls, filepath.Join(stackDir, "data", "subfolder"), 1000, 1001)
}

func TestExplicitManagedChownOverridesStackFileOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is not supported on Windows")
	}
	root := t.TempDir()
	calls := recordChown(t)
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute, StackFileOwner: &fileOwner{UID: 1000, GID: 1000}}}

	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", []StackManagedConfigFolder{{Path: "data", Chown: "2000:2001"}}, []managedFileWrite{{Path: "test.conf", Content: "x", Chown: "3000:3001"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	assertChownCall(t, *calls, filepath.Join(stackDir, "data"), 2000, 2001)
	assertChownCall(t, *calls, filepath.Join(stackDir, "test.conf"), 3000, 3001)
	assertNoChownCall(t, *calls, filepath.Join(stackDir, "data"), 1000, 1000)
	assertNoChownCall(t, *calls, filepath.Join(stackDir, "test.conf"), 1000, 1000)
}

func TestStackFileOwnerAppliedOnUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is not supported on Windows")
	}
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	calls := recordChown(t)
	app.cfg.StackFileOwner = &fileOwner{UID: 1000, GID: 1001}
	stack := Stack{Name: "web", AbsolutePath: filepath.Join(root, "web"), ComposeFile: "docker-compose.yml"}

	if err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx:1.27", []StackManagedConfigFolder{{Path: "data"}}, []managedFileWrite{{Path: "test.conf", Content: "x"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	assertChownCall(t, *calls, filepath.Join(stackDir, "docker-compose.yml"), 1000, 1001)
	assertChownCall(t, *calls, filepath.Join(stackDir, stackManagedConfigFile), 1000, 1001)
	assertChownCall(t, *calls, filepath.Join(stackDir, "data"), 1000, 1001)
	assertChownCall(t, *calls, filepath.Join(stackDir, "test.conf"), 1000, 1001)
}

func TestManagedFolderRejectsSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink handling differs on Windows")
	}
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stackDir, "data")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "compose.yml"}

	err = app.updateStackFiles(stack, "services:\n  app:\n    image: nginx", []StackManagedConfigFolder{{Path: "data", Chmod: "0700"}}, nil, StackManagedSettings{})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != originalInfo.Mode().Perm() {
		t.Fatalf("expected outside folder mode not to be changed through symlink, got %v want %v", info.Mode().Perm(), originalInfo.Mode().Perm())
	}
}

func TestManagedFolderRejectsSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink handling differs on Windows")
	}
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stackDir, "data")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "compose.yml"}

	err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx", []StackManagedConfigFolder{{Path: "data/subfolder"}}, nil, StackManagedSettings{})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink parent rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "subfolder")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected outside subfolder not to be created through symlink parent, got %v", err)
	}
}

func TestManagedFileRejectsSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink handling differs on Windows")
	}
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	outsideFile := filepath.Join(root, "outside.conf")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(stackDir, "test.conf")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	err := writeManagedFiles(stackDir, nil, []managedFileWrite{{Path: "test.conf", Content: "changed"}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
	content, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("expected outside file not to be changed, got %q", string(content))
	}
}

func TestManagedFileRejectsSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink handling differs on Windows")
	}
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stackDir, "data")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	err := writeManagedFiles(stackDir, managedTestFolders("data"), []managedFileWrite{{Path: "data/file.conf", Content: "changed"}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink parent rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "file.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected outside file not to be created through symlink parent, got %v", err)
	}
}

func TestManagedConfigRejectsSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink handling differs on Windows")
	}
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	outsideFile := filepath.Join(root, "outside-config.yml")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(stackDir, stackManagedConfigFile)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}

	if _, err := app.loadManagedStackConfig(stackDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected managed config read symlink rejection, got %v", err)
	}
	if err := writeStackManagedConfig(stackDir, StackManagedConfig{Settings: StackManagedSettings{ExternalBindMountsAllowed: true}}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected managed config write symlink rejection, got %v", err)
	}
	content, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("expected outside config not to be changed, got %q", string(content))
	}
}

func TestUpdateStackRejectsSymlinkComposeFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink handling differs on Windows")
	}
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	outsideFile := filepath.Join(root, "outside-compose.yml")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(stackDir, "compose.yml")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "compose.yml"}

	err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected compose file symlink rejection, got %v", err)
	}
	content, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("expected outside compose not to be changed, got %q", string(content))
	}
}

func TestCreateStackRejectsManagedFileMissingParent(t *testing.T) {
	root := t.TempDir()
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		t.Fatalf("runner should not be called when managed file validation fails")
		return "", nil
	}

	values := url.Values{}
	values.Set("name", "managed-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest")
	values.Add("managed_file_path", "missing/file.conf")
	values.Add("managed_file_content", "child=true\n")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "managed file parent folder does not exist: missing") {
		t.Fatalf("expected missing parent error, code=%d body=%s", rec.Code, rec.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'managed-stack'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected rejected managed stack not to be persisted, got count %d", count)
	}
	if _, err := os.Stat(filepath.Join(root, "managed-stack")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected rejected managed stack folder to be removed, stat err=%v", err)
	}
}

func TestInvalidManagedConfigBreaksStackLoading(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "legacy")
	mustWriteCompose(t, stackDir)
	if err := os.WriteFile(filepath.Join(stackDir, stackManagedConfigFile), []byte("folder data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('legacy', 'legacy', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}

	if _, err := app.loadStack("legacy", false); err == nil {
		t.Fatal("expected invalid managed config to break single stack load")
	}
	if _, err := app.loadStacks(false); err == nil {
		t.Fatal("expected invalid managed config to break stack list load")
	}
}

func TestEditStackRejectsInvalidManagedConfig(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "legacy")
	mustWriteCompose(t, stackDir)
	if err := os.WriteFile(filepath.Join(stackDir, stackManagedConfigFile), []byte("folder data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('legacy', 'legacy', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}

	pageReq := httptest.NewRequest(http.MethodGet, "http://docklight.local/stacks/legacy/edit", nil)
	pageReq.SetPathValue("name", "legacy")
	pageRec := httptest.NewRecorder()
	app.editStackPage(pageRec, pageReq)
	if pageRec.Code != http.StatusNotFound {
		t.Fatalf("expected edit page to reject invalid managed config, code=%d body=%s", pageRec.Code, pageRec.Body.String())
	}
}

func TestEditStackCreatesMissingManagedConfig(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "legacy")
	mustWriteCompose(t, stackDir)
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('legacy', 'legacy', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:changed")
	values.Set("external_bind_mounts_allowed", "on")
	values.Set("update_mode", "save")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/legacy/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "legacy")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected edit save redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}
	managedConfig, err := app.loadManagedStackConfig(stackDir)
	if err != nil {
		t.Fatal(err)
	}
	if !managedConfig.Settings.ExternalBindMountsAllowed {
		t.Fatalf("expected missing managed config to be created with settings: %#v", managedConfig)
	}
}

func TestCreateStackRejectsHostPIDByDefault(t *testing.T) {
	root := t.TempDir()
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		t.Fatalf("runner should not be called when validation fails")
		return "", nil
	}

	values := url.Values{}
	values.Set("name", "pid-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    pid: host")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "blocked host-level option: pid") {
		t.Fatalf("expected validation error, code=%d body=%s", rec.Code, rec.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'pid-stack'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected rejected stack not to be persisted, got count %d", count)
	}
}

func TestCreateStackRejectsPrivilegedByDefault(t *testing.T) {
	root := t.TempDir()
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		t.Fatalf("runner should not be called when validation fails")
		return "", nil
	}

	values := url.Values{}
	values.Set("name", "privileged-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    privileged: true")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "blocked host-level option: privileged") {
		t.Fatalf("expected validation error, code=%d body=%s", rec.Code, rec.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'privileged-stack'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected rejected stack not to be persisted, got count %d", count)
	}
}

func TestCreateStackRejectsSecurityOptByDefault(t *testing.T) {
	root := t.TempDir()
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		t.Fatalf("runner should not be called when validation fails")
		return "", nil
	}

	values := url.Values{}
	values.Set("name", "security-opt-stack")
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:latest\n    security_opt:\n      - no-new-privileges:true")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/new", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.createStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "blocked host-level option: security_opt") {
		t.Fatalf("expected validation error, code=%d body=%s", rec.Code, rec.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stacks WHERE name = 'security-opt-stack'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected rejected stack not to be persisted, got count %d", count)
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
	values.Add("managed_folder_path", "config")
	values.Add("managed_folder_chmod", "")
	values.Add("managed_folder_chown", "")
	values.Add("managed_folder_path", "data")
	values.Add("managed_folder_chmod", "")
	values.Add("managed_folder_chown", "")
	values.Set("external_bind_mounts_allowed", "on")
	values.Set("docker_socket_mounts_allowed", "on")
	values.Set("usb_device_bind_mounts_allowed", "on")
	values.Set("dbus_bind_mount_allowed", "on")
	values.Set("host_pid_allowed", "on")
	values.Set("privileged_allowed", "on")
	values.Set("security_opt_allowed", "on")
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
	managedConfig, err := app.loadManagedStackConfig(stackDir)
	if err != nil {
		t.Fatal(err)
	}
	if !managedConfig.Settings.ExternalBindMountsAllowed || !managedConfig.Settings.DockerSocketMountsAllowed || !managedConfig.Settings.USBDeviceBindMountsAllowed || !managedConfig.Settings.DBusBindMountAllowed || !managedConfig.Settings.HostPIDAllowed || !managedConfig.Settings.PrivilegedAllowed || !managedConfig.Settings.SecurityOptAllowed {
		t.Fatalf("expected stack settings in managed config: %#v", managedConfig.Settings)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "config")); err != nil {
		t.Fatalf("expected new bind folder to be created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data", "keep.txt")); err != nil {
		t.Fatalf("expected save-only to preserve existing folder contents: %v", err)
	}
}

func TestEditStackPreservesListedFolderContents(t *testing.T) {
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
	values.Add("managed_folder_path", "data")
	values.Add("managed_folder_chmod", "")
	values.Add("managed_folder_chown", "")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data", "old.txt")); err != nil {
		t.Fatalf("expected existing listed folder contents to be preserved: %v", err)
	}
}

func TestEditStackRejectsManagedFileMissingParentBeforeWritingCompose(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "editable")
	mustWriteCompose(t, stackDir)
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('editable', 'editable', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(stackDir, "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:changed")
	values.Add("managed_file_path", "missing/file.conf")
	values.Add("managed_file_content", "child=true\n")
	values.Set("update_mode", "save")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "managed file parent folder does not exist: missing") {
		t.Fatalf("expected missing parent error, code=%d body=%s", rec.Code, rec.Body.String())
	}
	current, err := os.ReadFile(filepath.Join(stackDir, "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatalf("expected compose file to remain unchanged, got %q", string(current))
	}
}

func TestStackManagedEnvFilePersistsWhenStoppedManagingWithoutDelete(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx:${TAG}", nil, []managedFileWrite{{Path: ".env", Content: "TAG=1.27\n"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "web", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "TAG=1.27\n" {
		t.Fatalf("unexpected env file content %q", string(content))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(root, "web", ".env"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("expected managed .env to be private, mode=%v", info.Mode().Perm())
		}
	}
	stack := Stack{Name: "web", AbsolutePath: filepath.Join(root, "web"), ComposeFile: "docker-compose.yml"}
	if err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "web", ".env")); err != nil {
		t.Fatalf("expected previously managed .env to remain unless explicitly deleted, got %v", err)
	}
}

func TestUpdateStackDeletesRemovedManagedEnvFileWhenRequested(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx:${TAG}", nil, []managedFileWrite{{Path: ".env", Content: "TAG=1.27\n"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stack := Stack{Name: "web", AbsolutePath: filepath.Join(root, "web"), ComposeFile: "docker-compose.yml"}

	if err := app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}, managedPathDeletes{Files: []string{".env"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "web", ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected explicitly deleted .env to be removed, got %v", err)
	}
}

func TestUpdateStackDeletesRemovedManagedFileWhenRequested(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", nil, []managedFileWrite{{Path: "test.conf", Content: "delete"}, {Path: "keep.conf", Content: "keep"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	if err := app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx", nil, []managedFileWrite{{Path: "keep.conf", Content: "keep"}}, StackManagedSettings{}, managedPathDeletes{Files: []string{"test.conf"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "test.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected removed managed file to be deleted, got %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(stackDir, "keep.conf")); err != nil || string(content) != "keep" {
		t.Fatalf("expected remaining managed file to be preserved, content=%q err=%v", string(content), err)
	}
}

func TestEditStackDeletesRemovedManagedFileFromForm(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	mustWriteCompose(t, stackDir)
	if err := os.WriteFile(filepath.Join(stackDir, "test.conf"), []byte("delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeStackManagedConfig(stackDir, StackManagedConfig{Files: []StackManagedConfigFile{{Path: "test.conf"}}}); err != nil {
		t.Fatal(err)
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "ok", nil
	}

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx")
	values.Set("delete_managed_file_path", "test.conf")
	values.Set("update_mode", "save")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "web")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected edit save redirect, code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(stackDir, "test.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected form-requested managed file delete, got %v", err)
	}
}

func TestUpdateStackDeletesRemovedManagedFolderWhenRequested(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", managedTestFolders("data"), []managedFileWrite{{Path: "data/file.conf", Content: "delete"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	if err := app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}, managedPathDeletes{Folders: []string{"data"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected removed managed folder to be deleted, got %v", err)
	}
}

func TestUpdateStackPreservesRemovedManagedPathsWithoutDeleteRequest(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", managedTestFolders("data"), []managedFileWrite{{Path: "test.conf", Content: "keep"}, {Path: "data/file.conf", Content: "keep"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	if err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"test.conf", filepath.Join("data", "file.conf")} {
		if _, err := os.Stat(filepath.Join(stackDir, path)); err != nil {
			t.Fatalf("expected removed managed path to remain without delete request: %s: %v", path, err)
		}
	}
}

func TestUpdateStackRejectsDeleteRequestForUnmanagedPath(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	if err := os.WriteFile(filepath.Join(stackDir, "manual.conf"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalCompose, err := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	err = app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx:changed", nil, nil, StackManagedSettings{}, managedPathDeletes{Files: []string{"manual.conf"}})
	if err == nil || !strings.Contains(err.Error(), "delete request must target a removed managed file") {
		t.Fatalf("expected unmanaged delete request to be rejected, got %v", err)
	}
	currentCompose, err := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(currentCompose) != string(originalCompose) {
		t.Fatalf("expected invalid delete request to leave compose unchanged, got %q", string(currentCompose))
	}
	if content, err := os.ReadFile(filepath.Join(stackDir, "manual.conf")); err != nil || string(content) != "keep" {
		t.Fatalf("expected unmanaged file to be preserved, content=%q err=%v", string(content), err)
	}
}

func TestUpdateStackRejectsFolderDeleteWithManagedChild(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", managedTestFolders("data"), []managedFileWrite{{Path: "data/file.conf", Content: "keep"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	err := app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx", nil, []managedFileWrite{{Path: "data/file.conf", Content: "keep"}}, StackManagedSettings{}, managedPathDeletes{Folders: []string{"data"}})
	if err == nil || !strings.Contains(err.Error(), "delete request would remove a still-managed file") {
		t.Fatalf("expected managed child protection, got %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(stackDir, "data", "file.conf")); err != nil || string(content) != "keep" {
		t.Fatalf("expected managed child file to be preserved, content=%q err=%v", string(content), err)
	}
}

func TestUpdateStackRejectsFolderDeleteWithExactManagedFile(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(filepath.Join(stackDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services:\n  app:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeStackManagedConfig(stackDir, StackManagedConfig{Folders: []StackManagedConfigFolder{{Path: "data"}}}); err != nil {
		t.Fatal(err)
	}
	originalCompose, err := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	err = app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx:changed", nil, []managedFileWrite{{Path: "data", Content: "file"}}, StackManagedSettings{}, managedPathDeletes{Folders: []string{"data"}})
	if err == nil || !strings.Contains(err.Error(), "delete request would remove a still-managed file") {
		t.Fatalf("expected exact managed file protection, got %v", err)
	}
	currentCompose, err := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(currentCompose) != string(originalCompose) {
		t.Fatalf("expected exact managed file validation to leave compose unchanged, got %q", string(currentCompose))
	}
}

func TestUpdateStackRejectsDeleteManagedFileSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink handling differs on Windows")
	}
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	outsideFile := filepath.Join(root, "outside.conf")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services:\n  app:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalCompose, err := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(stackDir, "test.conf")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := writeStackManagedConfig(stackDir, StackManagedConfig{Files: []StackManagedConfigFile{{Path: "test.conf"}}}); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	err = app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx:changed", nil, nil, StackManagedSettings{}, managedPathDeletes{Files: []string{"test.conf"}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink delete rejection, got %v", err)
	}
	currentCompose, err := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(currentCompose) != string(originalCompose) {
		t.Fatalf("expected delete target preflight failure to leave compose unchanged, got %q", string(currentCompose))
	}
	if content, err := os.ReadFile(outsideFile); err != nil || string(content) != "keep" {
		t.Fatalf("expected outside symlink target to be preserved, content=%q err=%v", string(content), err)
	}
}

func TestUpdateStackRejectsCaseOnlyDeleteOfCurrentManagedFile(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", nil, []managedFileWrite{{Path: "test.conf", Content: "keep"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	err := app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx", nil, []managedFileWrite{{Path: "TEST.conf", Content: "keep"}}, StackManagedSettings{}, managedPathDeletes{Files: []string{"test.conf"}})
	if err == nil || !strings.Contains(err.Error(), "delete request must target a removed managed file") {
		t.Fatalf("expected case-only retained file delete to be rejected, got %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(stackDir, "test.conf")); err != nil || string(content) != "keep" {
		t.Fatalf("expected case-only current managed file to be preserved, content=%q err=%v", string(content), err)
	}
}

func TestUpdateStackRejectsCaseVariantDeleteOfUnmanagedFile(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", nil, []managedFileWrite{{Path: "test.conf", Content: "managed"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "web")
	if err := os.WriteFile(filepath.Join(stackDir, "TEST.conf"), []byte("unmanaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	stack := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "docker-compose.yml"}

	err := app.updateStackFilesWithDeletes(stack, "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}, managedPathDeletes{Files: []string{"TEST.conf"}})
	if err == nil || !strings.Contains(err.Error(), "delete request must target a removed managed file") {
		t.Fatalf("expected case-variant unmanaged delete to be rejected, got %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(stackDir, "TEST.conf")); err != nil || string(content) != "unmanaged" {
		t.Fatalf("expected unmanaged case-variant file to be preserved, content=%q err=%v", string(content), err)
	}
}

func TestStackManualEnvFilePreservedWhenUnmanaged(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	if err := app.writeStackFiles("web", "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", ".env"), []byte("TAG=manual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stack := Stack{Name: "web", AbsolutePath: filepath.Join(root, "web"), ComposeFile: "docker-compose.yml"}
	if err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx:latest", nil, nil, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "web", ".env"))
	if err != nil || string(content) != "TAG=manual\n" {
		t.Fatalf("expected unmanaged .env to be preserved, content=%q err=%v", string(content), err)
	}
}

func TestEditStackValidationIgnoresRemovedManagedEnvFile(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	mustWriteCompose(t, stackDir)
	if err := os.WriteFile(filepath.Join(stackDir, ".env"), []byte("TAG=1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeStackManagedConfig(stackDir, StackManagedConfig{Files: []StackManagedConfigFile{{Path: ".env"}}}); err != nil {
		t.Fatal(err)
	}
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "container ls") {
			return "", nil
		}
		for i, arg := range args {
			if arg == "--env-file" && i+1 < len(args) {
				content, err := os.ReadFile(args[i+1])
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != "\n" {
					t.Fatalf("expected validation to ignore removed managed .env, content=%q", string(content))
				}
				return "required variable TAG is missing", errors.New("compose failed")
			}
		}
		t.Fatal("expected validation to pass an explicit empty --env-file")
		return "", nil
	}

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:${TAG:?required}")
	values.Set("delete_managed_file_path", "./.env")
	values.Set("update_mode", "save")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/web/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "web")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "required variable TAG is missing") {
		t.Fatalf("expected validation failure before stale .env removal, code=%d body=%s", rec.Code, rec.Body.String())
	}
	if content, err := os.ReadFile(filepath.Join(stackDir, ".env")); err != nil || string(content) != "TAG=1.27\n" {
		t.Fatalf("expected failed validation to preserve old .env, content=%q err=%v", string(content), err)
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
		{name: "bad bind", form: url.Values{"name": {"web"}, "compose_yaml": {"services:\n  app:\n    image: nginx"}, "managed_folder_path": {"../data"}}, want: "bind folder must be relative"},
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

	if err := app.writeStackFiles("files", "services:\n  app:\n    image: nginx", managedTestFolders("data", "data/subfolder"), []managedFileWrite{{Path: "test.conf", Content: "root=true\n"}, {Path: "data/file.conf", Content: "child=true\n"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	stack := Stack{Name: "files", AbsolutePath: filepath.Join(root, "files"), ComposeFile: "docker-compose.yml"}
	if err := app.updateStackFiles(stack, "services:\n  app:\n    image: nginx:1.27", managedTestFolders("data"), []managedFileWrite{{Path: "test.conf", Content: "root=false\n"}}, StackManagedSettings{}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "files", "test.conf"))
	if err != nil || string(content) != "root=false\n" {
		t.Fatalf("expected managed test.conf to be updated, content=%q err=%v", string(content), err)
	}
	if _, err := os.Stat(filepath.Join(root, "files", "data", "subfolder")); err != nil {
		t.Fatalf("expected nested managed folder: %v", err)
	}
	managedConfig, err := os.ReadFile(filepath.Join(root, "files", stackManagedConfigFile))
	if err != nil || !strings.Contains(string(managedConfig), "test.conf") {
		t.Fatalf("expected managed config with test.conf, content=%s err=%v", string(managedConfig), err)
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
	values.Add("managed_folder_path", "data")
	values.Add("managed_folder_chmod", "")
	values.Add("managed_folder_chown", "")
	values.Add("managed_folder_path", "config")
	values.Add("managed_folder_chmod", "")
	values.Add("managed_folder_chown", "")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)

	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/stacks/editable?operation=") {
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
	waitForActivityStatus(t, app, "editable", "update stack", "success")
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "down --rmi all --remove-orphans") || !strings.Contains(joined, " pull") || !strings.Contains(joined, "up -d --force-recreate --remove-orphans") {
		t.Fatalf("expected pull then safe force recreate without destructive down, calls:\n%s", joined)
	}
	var startedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM activity_log WHERE stack_name = 'editable' AND action = 'update stack' AND status = 'success'`).Scan(&startedCount); err != nil {
		t.Fatal(err)
	}
	if startedCount != 1 {
		t.Fatalf("expected success activity for update, got %d", startedCount)
	}
}

func TestEditStackRemoveImagesOptionUsesDestructiveRecreate(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "editable")
	mustWriteCompose(t, stackDir)
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
	values.Set("remove_images", "on")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect to stack detail, code=%d", rec.Code)
	}
	waitForActivityStatus(t, app, "editable", "update stack", "success")
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "down --rmi all --remove-orphans") || !strings.Contains(joined, "up -d --force-recreate") {
		t.Fatalf("expected destructive down then force recreate, calls:\n%s", joined)
	}
	if strings.Contains(joined, "up -d --force-recreate --remove-orphans") {
		t.Fatalf("destructive option should use the explicit down/up path, calls:\n%s", joined)
	}
}

func TestEditStackPullFailureDoesNotStopStack(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "editable")
	mustWriteCompose(t, stackDir)
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('editable', 'editable', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if strings.HasSuffix(call, " pull") {
			return "pull failed", errTestBackend
		}
		return "ok", nil
	}

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:1.27")
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected failure redirect, code=%d", rec.Code)
	}
	waitForActivityStatus(t, app, "editable", "update stack", "failed")
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, " down ") || strings.Contains(joined, " up ") {
		t.Fatalf("pull failure should not stop or recreate the stack, calls:\n%s", joined)
	}
}

func TestEditStackDoesNotWriteFilesWhenOperationRunning(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "editable")
	mustWriteCompose(t, stackDir)
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('editable', 'editable', 'compose.yml')`); err != nil {
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

	startReq := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/editable/start", nil)
	startReq.SetPathValue("name", "editable")
	startReq.SetPathValue("action", "start")
	app.stackAction(httptest.NewRecorder(), startReq)
	<-started

	values := url.Values{}
	values.Set("compose_yaml", "services:\n  app:\n    image: nginx:changed")
	req := formRequest("http://docklight.local/stacks/editable/edit", values)
	req.SetPathValue("name", "editable")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected busy redirect, got %d", rec.Code)
	}
	composeBytes, err := os.ReadFile(filepath.Join(stackDir, "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(composeBytes), "nginx:changed") {
		t.Fatalf("compose file changed while operation was running: %s", string(composeBytes))
	}
	close(release)
	waitForActivityStatus(t, app, "editable", "start", "success")
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
		{name: "unsafe bind", path: "web", form: url.Values{"compose_yaml": {"services:\n  app:\n    image: nginx"}, "managed_folder_path": {"/host"}}, want: "bind folder"},
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

	req := formRequest("http://docklight.local/stacks/web/edit", url.Values{"compose_yaml": {"services:\n  app:\n    image: nginx:1.28"}, "remove_images": {"on"}})
	req.SetPathValue("name", "web")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected down failure redirect, got %d", rec.Code)
	}
	waitForActivityStatus(t, app, "web", "update stack", "failed")

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
