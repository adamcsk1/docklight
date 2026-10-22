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

func TestSessionOnlyDatabasePersistsAcrossRestart(t *testing.T) {
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
	if !a.validSession(a.signSessionID("persisted")) {
		t.Fatal("session lost on restart")
	}
	var tables string
	if err = db.QueryRow(`SELECT group_concat(name, ',') FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*'`).Scan(&tables); err != nil || tables != "sessions" {
		t.Fatalf("unexpected database tables: %q %v", tables, err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "sessions.db" {
		t.Fatalf("unexpected automatic backup/storage files: %v", files)
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
