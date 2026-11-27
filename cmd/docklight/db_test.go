package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionAndMaintenanceDatabasePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	a := &App{db: db, cfg: Config{SessionSecret: strings.Repeat("s", 32)}}
	if err = a.initDB(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	coordinator := a.coordinator
	if _, err = db.Exec(`INSERT INTO output_acknowledgements VALUES('local','http://agent','job-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO sessions(id_hash,expires_at) VALUES(?,?)`, a.sessionIDHash("persisted"), time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	a.db = db
	if err = a.initDB(); err != nil {
		t.Fatal(err)
	}
	if a.coordinator != coordinator {
		t.Fatal("coordinator identity changed after restart")
	}
	if !a.validSession(a.signSessionID("persisted")) {
		t.Fatal("session lost on restart")
	}
	var tables string
	if err = db.QueryRow(`SELECT group_concat(name, ',') FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*'`).Scan(&tables); err != nil || tables != "sessions,maintenance,output_acknowledgements,image_updates" {
		t.Fatalf("unexpected database tables: %q %v", tables, err)
	}
	var acknowledgements int
	if err = db.QueryRow(`SELECT COUNT(*) FROM output_acknowledgements WHERE host='local' AND endpoint='http://agent' AND job='job-1'`).Scan(&acknowledgements); err != nil || acknowledgements != 1 {
		t.Fatalf("acknowledgement lost after restart: %d %v", acknowledgements, err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "sessions.db" {
		t.Fatalf("unexpected automatic backup/storage files: %v", files)
	}
}

func TestInvalidImageUpdateSchemaRefusedBeforeMigration(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE sessions (id_hash TEXT PRIMARY KEY NOT NULL, expires_at TEXT NOT NULL); CREATE TABLE image_updates (unexpected TEXT)`); err != nil {
		t.Fatal(err)
	}
	app := &App{db: db}
	if err = app.initDB(); err == nil {
		t.Fatal("invalid image update schema accepted")
	}
	var tables int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*'`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("refused schema modified: %d %v", tables, err)
	}
}

func TestUnsupportedDatabaseIsRefusedWithoutChanges(t *testing.T) {
	for _, schema := range []string{
		`CREATE TABLE sessions(id_hash TEXT PRIMARY KEY, username TEXT NOT NULL, expires_at TEXT NOT NULL); INSERT INTO sessions VALUES('preserve','admin','future');`,
		`CREATE TABLE sessions(id TEXT PRIMARY KEY, expires_at TEXT); INSERT INTO sessions VALUES('preserve','future');`,
		`CREATE TABLE application_data(value TEXT); INSERT INTO application_data VALUES('preserve');`,
		`CREATE TABLE sessions(id_hash TEXT PRIMARY KEY, expires_at TEXT); CREATE TABLE another_table(value TEXT); INSERT INTO another_table VALUES('preserve');`,
	} {
		t.Run(schema[:min(45, len(schema))], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err = db.Exec(schema); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			a := &App{db: db}
			if err = a.initDB(); err == nil || !strings.Contains(err.Error(), "fresh sessions database") {
				t.Fatalf("unsupported schema accepted: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("incompatible database was modified")
			}
			files, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 1 {
				t.Fatal("unexpected automatic database migration or backup")
			}
		})
	}
}

func TestAcknowledgementMigrationPreservesExistingState(t *testing.T) {
	a := newTestApp(t)
	cookie := session(t, a)
	coordinator := a.coordinator
	if _, err := a.db.Exec(`DROP TABLE output_acknowledgements`); err != nil {
		t.Fatal(err)
	}
	if err := a.initDB(); err != nil {
		t.Fatal(err)
	}
	if !a.validSession(cookie.Value) || a.coordinator != coordinator {
		t.Fatal("migration changed existing session or coordinator")
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM output_acknowledgements`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration: %d %v", count, err)
	}
}

func TestInvalidDatabaseIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-database")
	content := []byte("important file, not a SQLite database")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = (&App{db: db}).initDB(); err == nil {
		t.Fatal("invalid database accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, after) {
		t.Fatal("invalid database file was overwritten")
	}
}
