package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationRemovesUpdateMetadata(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE stacks (
  name TEXT PRIMARY KEY,
  relative_path TEXT NOT NULL,
  compose_file TEXT NOT NULL,
  auto_update_enabled INTEGER NOT NULL DEFAULT 0,
  update_status TEXT NOT NULL DEFAULT 'unknown',
  update_checked_at TEXT NOT NULL DEFAULT '',
  update_fingerprint TEXT NOT NULL DEFAULT '',
  external_bind_mounts_allowed INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE app_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE image_updates (image_ref TEXT PRIMARY KEY, image_id TEXT NOT NULL);
INSERT INTO stacks (name, relative_path, compose_file, auto_update_enabled, update_status, external_bind_mounts_allowed) VALUES ('web', 'web', 'compose.yml', 1, 'available', 1);
INSERT INTO app_settings (key, value) VALUES ('update_check_interval', '1h'), ('auto_update_interval', '6h');
INSERT INTO image_updates (image_ref, image_id) VALUES ('nginx:latest', 'sha256:old');
`); err != nil {
		t.Fatal(err)
	}
	app := &App{db: db}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"auto_update_enabled", "update_status", "update_checked_at", "update_fingerprint"} {
		exists, err := app.tableHasColumn("stacks", column)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("expected %s to be removed", column)
		}
	}
	if exists, err := app.tableExists("image_updates"); err != nil || exists {
		t.Fatalf("expected image_updates table removed, exists=%v err=%v", exists, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM app_settings WHERE key IN ('update_check_interval', 'auto_update_interval')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected update settings removed, got %d", count)
	}
	var externalBinds, dockerSocketMounts, hostPID int
	if err := db.QueryRow(`SELECT external_bind_mounts_allowed, docker_socket_mounts_allowed, host_pid_allowed FROM stacks WHERE name = 'web'`).Scan(&externalBinds, &dockerSocketMounts, &hostPID); err != nil || externalBinds != 1 || dockerSocketMounts != 0 || hostPID != 0 {
		t.Fatalf("expected stack settings preserved with safe defaults, externalBinds=%d dockerSocketMounts=%d hostPID=%d err=%v", externalBinds, dockerSocketMounts, hostPID, err)
	}
}

func TestDeleteStackReportsDatabaseCleanupFailure(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "victim"))
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('victim', 'victim', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "docker container ls -aq" {
			return "", nil
		}
		if strings.Contains(call, "docker compose") && strings.Contains(call, " down --remove-orphans --volumes") {
			if _, err := db.Exec(`DROP TABLE stacks`); err != nil {
				t.Fatal(err)
			}
			return "removed", nil
		}
		return "", nil
	}
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/stacks/victim/delete", nil)
	req.SetPathValue("name", "victim")
	rec := httptest.NewRecorder()
	app.deleteStack(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect after db cleanup failure, got %d", rec.Code)
	}
}

func TestActionHandlersDatabaseErrorRedirects(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/database/rebuild", nil)
	rec := httptest.NewRecorder()
	app.rebuildDatabase(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected rebuild begin error redirect, got %d", rec.Code)
	}

}

func TestDBHelpersReportClosedDatabaseErrors(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := app.initDB(); err == nil {
		t.Fatal("expected initDB to fail on closed database")
	}
	if _, err := app.tableExists("sessions"); err == nil {
		t.Fatal("expected tableExists to fail on closed database")
	}
	if _, err := app.tableHasColumn("sessions", "id_hash"); err == nil {
		t.Fatal("expected tableHasColumn to fail on closed database")
	}
}
