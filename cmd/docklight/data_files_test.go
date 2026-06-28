package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite"
)

func TestDataFileOwnerAppliedToDataDirectoryAndDatabaseFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is not supported on Windows")
	}
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "docklight.db")
	calls := recordChown(t)
	cfg := Config{DBPath: dbPath, DataFileOwner: &fileOwner{UID: 2000, GID: 2001}}

	if err := ensureDataStorage(cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{cfg: cfg}
	if err := app.chownDataFiles(); err != nil {
		t.Fatal(err)
	}

	assertChownCall(t, *calls, dataDir, 2000, 2001)
	assertChownCall(t, *calls, dbPath, 2000, 2001)
	assertChownCall(t, *calls, dbPath+"-wal", 2000, 2001)
	assertChownCall(t, *calls, dbPath+"-shm", 2000, 2001)
}

func TestDataFileOwnerIgnoresMissingSQLiteSidecars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is not supported on Windows")
	}
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "docklight.db")
	calls := recordChown(t)
	if err := os.WriteFile(dbPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{DBPath: dbPath, DataFileOwner: &fileOwner{UID: 2000, GID: 2001}}}

	if err := app.chownDataFiles(); err != nil {
		t.Fatal(err)
	}

	assertChownCall(t, *calls, dbPath, 2000, 2001)
	assertNoChownCall(t, *calls, dbPath+"-wal", 2000, 2001)
	assertNoChownCall(t, *calls, dbPath+"-shm", 2000, 2001)
}

func TestDataFileOwnerAppliedAfterRealDatabaseInit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is not supported on Windows")
	}
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "docklight.db")
	calls := recordChown(t)
	cfg := Config{DBPath: dbPath, DataFileOwner: &fileOwner{UID: 2000, GID: 2001}}
	if err := ensureDataStorage(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := &App{cfg: cfg, db: db}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	if err := app.chownDataFiles(); err != nil {
		t.Fatal(err)
	}

	assertChownCall(t, *calls, dataDir, 2000, 2001)
	assertChownCall(t, *calls, dbPath, 2000, 2001)
	assertChownCall(t, *calls, filepath.Join(dataDir, "config.yml"), 2000, 2001)
}

func TestDataFileOwnerReturnsChownErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown is not supported on Windows")
	}
	dataDir := t.TempDir()
	wantErr := errors.New("chown failed")
	original := chownPath
	chownPath = func(path string, uid int, gid int) error {
		return wantErr
	}
	t.Cleanup(func() { chownPath = original })

	err := ensureDataStorage(Config{DBPath: filepath.Join(dataDir, "docklight.db"), DataFileOwner: &fileOwner{UID: 2000, GID: 2001}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected chown error, got %v", err)
	}
}
