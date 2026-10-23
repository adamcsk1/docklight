package main

import (
	"errors"
	"github.com/google/uuid"
)

func (app *App) initDB() error {
	if _, err := app.db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return err
	}
	var tables int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name NOT GLOB 'sqlite_*'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		if _, err := app.db.Exec(`CREATE TABLE IF NOT EXISTS sessions (id_hash TEXT PRIMARY KEY NOT NULL, expires_at TEXT NOT NULL)`); err != nil {
			return err
		}
	}
	var columns string
	if err := app.db.QueryRow(`SELECT COALESCE(group_concat(name, ','), '') FROM pragma_table_info('sessions')`).Scan(&columns); err != nil {
		return err
	}
	var unknown int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*' AND name NOT IN ('sessions','maintenance')`).Scan(&unknown); err != nil {
		return err
	}
	if unknown != 0 || columns != "id_hash,expires_at" {
		return errors.New("unsupported UI database schema: set DB_PATH to a fresh sessions database (default /data/sessions.db); existing data has not been changed")
	}
	// Additive migration leaves existing sessions intact; refuse unknown schemas first.
	if _, err := app.db.Exec(`CREATE TABLE IF NOT EXISTS maintenance (key TEXT PRIMARY KEY NOT NULL, value TEXT NOT NULL)`); err != nil {
		return err
	}
	var maintenanceColumns string
	if err := app.db.QueryRow(`SELECT group_concat(name, ',') FROM pragma_table_info('maintenance')`).Scan(&maintenanceColumns); err != nil {
		return err
	}
	if maintenanceColumns != "key,value" {
		return errors.New("unsupported maintenance database schema")
	}
	if _, err := app.db.Exec(`INSERT OR IGNORE INTO maintenance(key,value) VALUES('coordinator',?)`, uuid.NewString()); err != nil {
		return err
	}
	if err := app.db.QueryRow(`SELECT value FROM maintenance WHERE key='coordinator'`).Scan(&app.coordinator); err != nil {
		return err
	}
	if id, err := uuid.Parse(app.coordinator); err != nil || id.String() != app.coordinator {
		return errors.New("invalid persisted coordinator identity; restore the UI database backup")
	}
	return nil
}
