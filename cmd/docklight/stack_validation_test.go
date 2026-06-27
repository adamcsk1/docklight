package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunComposeRejectsUnsafeDatabasePaths(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		t.Fatal("runner should not be called for unsafe compose path")
		return "", nil
	}

	_, err := app.runCompose(Stack{AbsolutePath: filepath.Join(root, "stack"), ComposeFile: "../other/compose.yml"}, "ps")
	if err == nil || !strings.Contains(err.Error(), "unsafe compose file path") {
		t.Fatalf("expected unsafe compose path error, got %v", err)
	}

	_, err = app.runCompose(Stack{AbsolutePath: filepath.Dir(root), ComposeFile: "compose.yml"}, "ps")
	if err == nil || !strings.Contains(err.Error(), "outside stacks root") {
		t.Fatalf("expected outside root error, got %v", err)
	}
}

func TestValidateComposeYAMLRejectsUnsafeBindMounts(t *testing.T) {
	for _, value := range []string{
		"services:\n  app:\n    volumes:\n      - /:/host",
		"services:\n  app:\n    volumes:\n      - ../data:/data",
		"services:\n  app:\n    volumes:\n      - C:\\data:/data",
		"services:\n  app:\n    volumes:\n      - ${HOST_PATH}:/data",
		"services:\n  app:\n    volumes:\n      - type: bind\n        source: ./data\n        target: /data",
		"services:\n  app:\n    volumes:\n      - type: \"bind\"\n        source: ./data\n        target: /data",
		"services:\n  app:\n    privileged: \"true\"",
		"services:\n  app:\n    network_mode: \"host\"",
		"services:\n  app:\n    pid: \"host\"",
	} {
		if err := validateComposeYAML(value); err == nil {
			t.Fatalf("expected unsafe bind mount to be rejected: %s", value)
		}
	}
	if err := validateComposeYAML("services:\n  app:\n    volumes:\n      - data:/data"); err != nil {
		t.Fatalf("expected named volume to pass: %v", err)
	}
}

func TestValidateComposeDefinitionUsesResolvedEnvValues(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	called := false
	app.runner = func(dir, name string, args ...string) (string, error) {
		called = true
		joined := strings.Join(args, " ")
		if dir != stackDir || name != "docker" || !strings.Contains(joined, "--env-file") || !strings.Contains(joined, " config") {
			t.Fatalf("unexpected compose config call dir=%q name=%q args=%v", dir, name, args)
		}
		return "services:\n  app:\n    image: nginx:1.27\n    ports:\n      - mode: ingress\n        target: 80\n        published: \"8080\"\n", nil
	}

	err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG}\n    ports:\n      - ${HOST_PORT}:80", "TAG=1.27\nHOST_PORT=8080", true, false, false, false, false, false, false)
	if err != nil {
		t.Fatalf("expected resolved compose to pass: %v", err)
	}
	if !called {
		t.Fatal("expected docker compose config to be called")
	}
}

func TestValidateComposeDefinitionUsesManagedEmptyEnvFile(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, ".env"), []byte("TAG=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		for i, arg := range args {
			if arg == "--env-file" && i+1 < len(args) {
				content, err := os.ReadFile(args[i+1])
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != "\n" {
					t.Fatalf("expected managed empty .env temp file, content=%q", string(content))
				}
				return "services:\n  app:\n    image: nginx\n", nil
			}
		}
		t.Fatal("expected validation to pass an explicit --env-file for managed empty .env")
		return "", nil
	}

	if err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG:-latest}", "", true, false, false, false, false, false, false); err != nil {
		t.Fatalf("expected managed empty .env to validate with explicit env file: %v", err)
	}
}

func TestValidateComposeDefinitionRejectsUnsafeResolvedEnvValues(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "services:\n  app:\n    image: nginx\n    network_mode: host\n", nil
	}

	err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx\n    network_mode: ${NETWORK_MODE}", "NETWORK_MODE=host", true, false, false, false, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "network_mode") {
		t.Fatalf("expected resolved host network mode to be rejected, got %v", err)
	}
}

func TestValidateComposeDefinitionAllowsHostPIDWhenEnabled(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "services:\n  app:\n    image: nginx\n    pid: host\n", nil
	}

	if err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx\n    pid: ${PID_MODE}", "PID_MODE=host", true, false, false, false, false, true, false); err != nil {
		t.Fatalf("expected host PID mode to pass when enabled: %v", err)
	}
	err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx\n    pid: ${PID_MODE}", "PID_MODE=host", true, false, false, false, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("expected host PID mode to be rejected when disabled, got %v", err)
	}
}

func TestValidateComposeYAMLHostPIDAllowanceDoesNotAllowOtherHostOptions(t *testing.T) {
	if err := validateComposeYAMLWithExternalBinds("services:\n  app:\n    image: nginx\n    pid: host", false, false, false, false, true, false); err != nil {
		t.Fatalf("expected host PID mode to pass when enabled: %v", err)
	}
	for _, value := range []string{"network_mode: host", "ipc: host", "cgroup: host", "userns_mode: host"} {
		if err := validateComposeYAMLWithExternalBinds(value, false, false, false, false, true, false); err == nil {
			t.Fatalf("expected %q to stay rejected", value)
		}
	}
}

func TestValidateComposeDefinitionAllowsResolvedBindInsideStack(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return fmt.Sprintf("services:\n  app:\n    image: nginx:1.27\n    volumes:\n      - type: bind\n        source: %s\n        target: /data\n", filepath.ToSlash(filepath.Join(stackDir, "data"))), nil
	}

	err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG}\n    volumes:\n      - ./data:/data", "TAG=1.27", true, false, false, false, false, false, false)
	if err != nil {
		t.Fatalf("expected resolved bind inside stack to pass: %v", err)
	}
}

func TestValidateComposeDefinitionRejectsResolvedBindOutsideStack(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return fmt.Sprintf("services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: %s\n        target: /data\n", filepath.ToSlash(filepath.Join(root, "outside"))), nil
	}

	err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx\n    volumes:\n      - ${DATA_DIR}:/data", "DATA_DIR=../outside", true, false, false, false, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe host bind mount") || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected resolved bind outside stack to be rejected, got %v", err)
	}
}

func TestValidateComposeDefinitionAllowsImmichExternalBindsWhenEnabled(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "immich")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	resolved := "services:\n  immich-server:\n    image: ghcr.io/immich-app/immich-server:v2\n    volumes:\n      - type: bind\n        source: /srv/immich/upload\n        target: /usr/src/app/upload\n      - type: bind\n        source: /etc/localtime\n        target: /etc/localtime\n        read_only: true\n  database:\n    image: ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0\n    volumes:\n      - type: bind\n        source: /srv/immich/db\n        target: /var/lib/postgresql/data\nvolumes:\n  model-cache: {}\n"
	app.runner = func(dir, name string, args ...string) (string, error) {
		return resolved, nil
	}
	compose := "services:\n  immich-server:\n    image: ghcr.io/immich-app/immich-server:${IMMICH_VERSION:-v2}\n    volumes:\n      - ${UPLOAD_LOCATION}:/usr/src/app/upload\n      - /etc/localtime:/etc/localtime:ro\n    env_file:\n      - .env\n  database:\n    image: ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0\n    environment:\n      POSTGRES_PASSWORD: ${DB_PASSWORD}\n    volumes:\n      - ${DB_DATA_LOCATION}:/var/lib/postgresql/data\nvolumes:\n  model-cache:\n"
	envFile := "UPLOAD_LOCATION=/srv/immich/upload\nDB_DATA_LOCATION=/srv/immich/db\nDB_PASSWORD=secret\n"

	if err := app.validateComposeDefinition("immich", stackDir, compose, envFile, true, true, false, false, false, false, false); err != nil {
		t.Fatalf("expected Immich-style external binds to pass when enabled: %v", err)
	}
	err := app.validateComposeDefinition("immich", stackDir, compose, envFile, true, false, false, false, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "/srv/immich/upload") {
		t.Fatalf("expected external bind path to be reported when disabled, got %v", err)
	}
}

func TestValidateComposeDefinitionBlocksDockerSocketEvenWithExternalBinds(t *testing.T) {
	for _, source := range []string{"/var/run/docker.sock", "/var/run//docker.sock", "/run/docker.sock", "/var/run", "/run"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			stackDir := filepath.Join(root, "web")
			if err := os.MkdirAll(stackDir, 0o755); err != nil {
				t.Fatal(err)
			}
			app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
			app.runner = func(dir, name string, args ...string) (string, error) {
				return fmt.Sprintf("services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: %s\n        target: /sock\n", source), nil
			}

			err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG}\n    volumes:\n      - "+source+":/sock", "TAG=latest", true, true, false, false, false, false, false)
			if err == nil || !strings.Contains(err.Error(), "docker socket") {
				t.Fatalf("expected docker socket exposure to stay blocked for %q, got %v", source, err)
			}
		})
	}
}

func TestValidateComposeDefinitionAllowsExactDockerSocketWhenEnabled(t *testing.T) {
	for _, source := range []string{"/var/run/docker.sock", "/var/run//docker.sock", "/run/docker.sock"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			stackDir := filepath.Join(root, "web")
			if err := os.MkdirAll(stackDir, 0o755); err != nil {
				t.Fatal(err)
			}
			app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
			app.runner = func(dir, name string, args ...string) (string, error) {
				return fmt.Sprintf("services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: %s\n        target: /sock\n", source), nil
			}

			if err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG}\n    volumes:\n      - "+source+":/sock", "TAG=latest", true, false, true, false, false, false, false); err != nil {
				t.Fatalf("expected exact docker socket mount to pass for %q: %v", source, err)
			}
		})
	}
}

func TestValidateComposeDefinitionAllowsExactUSBDeviceBindWhenEnabled(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: /dev/bus/usb\n        target: /dev/bus/usb\n", nil
	}

	compose := "services:\n  app:\n    image: nginx:${TAG}\n    volumes:\n      - /dev/bus/usb:/dev/bus/usb"
	if err := app.validateComposeDefinition("web", stackDir, compose, "TAG=latest", true, false, false, true, false, false, false); err != nil {
		t.Fatalf("expected exact USB bind mount to pass when enabled: %v", err)
	}
	err := app.validateComposeDefinition("web", stackDir, compose, "TAG=latest", true, false, false, false, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe host bind mount") {
		t.Fatalf("expected USB bind mount to be rejected when disabled, got %v", err)
	}
}

func TestValidateComposeDefinitionUSBDeviceBindDoesNotAllowOtherDeviceBinds(t *testing.T) {
	for _, source := range []string{"/dev", "/dev/bus", "/dev/ttyUSB0"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			stackDir := filepath.Join(root, "web")
			if err := os.MkdirAll(stackDir, 0o755); err != nil {
				t.Fatal(err)
			}
			app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
			app.runner = func(dir, name string, args ...string) (string, error) {
				return fmt.Sprintf("services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: %s\n        target: /device\n", source), nil
			}

			err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG}\n    volumes:\n      - "+source+":/device", "TAG=latest", true, false, false, true, false, false, false)
			if err == nil || !strings.Contains(err.Error(), "unsafe host bind mount") {
				t.Fatalf("expected unsafe device bind to stay blocked for %q, got %v", source, err)
			}
		})
	}
}

func TestValidateComposeDefinitionAllowsExactDBusBindWhenEnabled(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: /run/dbus\n        target: /run/dbus\n", nil
	}

	compose := "services:\n  app:\n    image: nginx:${TAG}\n    volumes:\n      - /run/dbus:/run/dbus"
	if err := app.validateComposeDefinition("web", stackDir, compose, "TAG=latest", true, false, false, false, true, false, false); err != nil {
		t.Fatalf("expected exact DBus bind mount to pass when enabled: %v", err)
	}
	err := app.validateComposeDefinition("web", stackDir, compose, "TAG=latest", true, false, false, false, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe host bind mount") {
		t.Fatalf("expected DBus bind mount to be rejected when disabled, got %v", err)
	}
}

func TestValidateComposeDefinitionDBusBindDoesNotAllowOtherRunBinds(t *testing.T) {
	for _, source := range []string{"/run", "/run/dbus/system_bus_socket"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			stackDir := filepath.Join(root, "web")
			if err := os.MkdirAll(stackDir, 0o755); err != nil {
				t.Fatal(err)
			}
			app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
			app.runner = func(dir, name string, args ...string) (string, error) {
				return fmt.Sprintf("services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: %s\n        target: /dbus\n", source), nil
			}

			err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG}\n    volumes:\n      - "+source+":/dbus", "TAG=latest", true, false, false, false, true, false, false)
			if err == nil {
				t.Fatalf("expected unsafe DBus-adjacent bind to stay blocked for %q, got %v", source, err)
			}
		})
	}
}

func TestValidateComposeDefinitionBlocksDockerSocketDirectoriesWhenEnabled(t *testing.T) {
	for _, source := range []string{"/var/run", "/run"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			stackDir := filepath.Join(root, "web")
			if err := os.MkdirAll(stackDir, 0o755); err != nil {
				t.Fatal(err)
			}
			app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
			app.runner = func(dir, name string, args ...string) (string, error) {
				return fmt.Sprintf("services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: %s\n        target: /sock\n", source), nil
			}

			err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG}\n    volumes:\n      - "+source+":/sock", "TAG=latest", true, true, true, false, false, false, false)
			if err == nil || !strings.Contains(err.Error(), "docker socket") {
				t.Fatalf("expected docker socket directory exposure to stay blocked for %q, got %v", source, err)
			}
		})
	}
}

func TestValidateComposeDefinitionReportsComposeConfigErrors(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "required variable TAG is missing", errors.New("compose failed")
	}

	err := app.validateComposeDefinition("web", stackDir, "services:\n  app:\n    image: nginx:${TAG:?required}", "", false, false, false, false, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "required variable TAG is missing") {
		t.Fatalf("expected compose config error to be reported, got %v", err)
	}
}

func TestStackUpdateAndDeleteRejectUnsafeDatabasePaths(t *testing.T) {
	root := t.TempDir()
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', '../compose.yml'), ('outside', '..', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "container ls") {
			return "", nil
		}
		return "ok", nil
	}

	req := formRequest("http://docklight.local/stacks/web/edit", url.Values{"compose_yaml": {"services:\n  app:\n    image: nginx"}})
	req.SetPathValue("name", "web")
	rec := httptest.NewRecorder()
	app.updateStack(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "outside stack folder") {
		t.Fatalf("expected unsafe compose path validation, code=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/outside/delete", nil)
	req.SetPathValue("name", "outside")
	rec = httptest.NewRecorder()
	app.deleteStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected unsafe delete path redirect, got %d", rec.Code)
	}
}

func TestStackSafetyPermissionsDefaultFalse(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.rescanStacks(); err != nil {
		t.Fatal(err)
	}
	stack, err := app.loadStack("web", false)
	if err != nil {
		t.Fatal(err)
	}
	if stack.ExternalBindMountsAllowed {
		t.Fatal("expected external bind mounts to default false")
	}
	if stack.DockerSocketMountsAllowed {
		t.Fatal("expected docker socket mounts to default false")
	}
	if stack.USBDeviceBindMountsAllowed {
		t.Fatal("expected USB device bind mounts to default false")
	}
	if stack.DBusBindMountAllowed {
		t.Fatal("expected DBus bind mount to default false")
	}
	if stack.HostPIDAllowed {
		t.Fatal("expected host PID namespace to default false")
	}
	if stack.PrivilegedAllowed {
		t.Fatal("expected privileged mode to default false")
	}
}

func TestStackFileHelpersRejectUnsafePaths(t *testing.T) {
	root := t.TempDir()
	app := &App{cfg: Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret"}}
	if err := app.writeStackFiles("Bad Name", "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}); err == nil {
		t.Fatal("expected invalid stack name to be rejected before writing files")
	}
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unsafe := Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "../compose.yml"}
	if err := app.updateStackFiles(unsafe, "services:\n  app:\n    image: nginx", nil, nil, StackManagedSettings{}); err == nil {
		t.Fatal("expected unsafe compose path to be rejected")
	}
}
