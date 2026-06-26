package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
)

func (app *App) initDB() error {
	_, err := app.db.Exec(`PRAGMA foreign_keys = ON;`)
	if err != nil {
		return err
	}
	return app.migrateDB()
}

func (app *App) migrateDB() error {
	_, err := app.db.Exec(`
CREATE TABLE IF NOT EXISTS stacks (
  name TEXT PRIMARY KEY,
  relative_path TEXT NOT NULL,
  compose_file TEXT NOT NULL,
  ignored INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS activity_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  stack_name TEXT NOT NULL,
  action TEXT NOT NULL,
  status TEXT NOT NULL,
  output TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS app_settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`)
	if err != nil {
		return err
	}
	if err := app.ensureSessionSchema(); err != nil {
		return err
	}
	if err := app.ensureDefaultSettings(); err != nil {
		return err
	}
	for _, stmt := range []string{
		`ALTER TABLE stacks ADD COLUMN ignored INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := app.db.Exec(stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	if err := app.migrateDBStackSettingsToConfig(); err != nil {
		return err
	}
	if err := app.cleanupStackSchema(); err != nil {
		return err
	}
	_, err = app.db.Exec(`PRAGMA user_version = 1`)
	return err
}

func (app *App) cleanupStackSchema() error {
	if _, err := app.db.Exec(`DELETE FROM app_settings WHERE key IN ('update_check_interval', 'auto_update_interval')`); err != nil {
		return err
	}
	if _, err := app.db.Exec(`DROP TABLE IF EXISTS image_updates`); err != nil {
		return err
	}
	needsRebuild := false
	for _, column := range []string{"auto_update_enabled", "update_status", "update_checked_at", "update_fingerprint", "external_bind_mounts_allowed", "docker_socket_mounts_allowed", "host_pid_allowed"} {
		exists, err := app.tableHasColumn("stacks", column)
		if err != nil {
			return err
		}
		needsRebuild = needsRebuild || exists
	}
	if !needsRebuild {
		return nil
	}
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
CREATE TABLE stacks_new (
  name TEXT PRIMARY KEY,
  relative_path TEXT NOT NULL,
  compose_file TEXT NOT NULL,
  ignored INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO stacks_new (name, relative_path, compose_file, ignored, created_at, updated_at)
SELECT name, relative_path, compose_file, ignored, created_at, updated_at FROM stacks;
DROP TABLE stacks;
ALTER TABLE stacks_new RENAME TO stacks;
`); err != nil {
		return err
	}
	return tx.Commit()
}

func (app *App) migrateDBStackSettingsToConfig() error {
	for _, column := range []string{"external_bind_mounts_allowed", "docker_socket_mounts_allowed", "host_pid_allowed"} {
		exists, err := app.tableHasColumn("stacks", column)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
	}
	rows, err := app.db.Query(`SELECT relative_path, external_bind_mounts_allowed, docker_socket_mounts_allowed, host_pid_allowed FROM stacks WHERE ignored = 0 AND (external_bind_mounts_allowed = 1 OR docker_socket_mounts_allowed = 1 OR host_pid_allowed = 1)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var relativePath string
		var externalBinds int
		var dockerSocketMounts int
		var hostPID int
		if err := rows.Scan(&relativePath, &externalBinds, &dockerSocketMounts, &hostPID); err != nil {
			return err
		}
		stackDir := filepath.Join(app.cfg.StacksRoot, relativePath)
		if err := app.ensureStackPathSafe(stackDir); err != nil {
			return err
		}
		info, err := os.Stat(stackDir)
		if err != nil || !info.IsDir() {
			continue
		}
		managedConfig, err := app.loadManagedStackConfig(stackDir)
		if err != nil {
			return err
		}
		managedConfig.Settings = StackManagedSettings{ExternalBindMountsAllowed: externalBinds == 1, DockerSocketMountsAllowed: dockerSocketMounts == 1, HostPIDAllowed: hostPID == 1}
		if err := writeStackManagedConfig(stackDir, managedConfig); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (app *App) ensureSessionSchema() error {
	exists, err := app.tableExists("sessions")
	if err != nil {
		return err
	}
	if exists {
		hasHash, err := app.tableHasColumn("sessions", "id_hash")
		if err != nil {
			return err
		}
		if !hasHash {
			if _, err := app.db.Exec(`DROP TABLE sessions`); err != nil {
				return err
			}
		}
	}
	_, err = app.db.Exec(`
CREATE TABLE IF NOT EXISTS sessions (
  id_hash TEXT PRIMARY KEY,
  username TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`)
	return err
}

func (app *App) tableExists(name string) (bool, error) {
	var count int
	err := app.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count)
	return count > 0, err
}

func (app *App) tableHasColumn(tableName, columnName string) (bool, error) {
	rows, err := app.db.Query(`PRAGMA table_info(` + tableName + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}
	return false, rows.Err()
}
