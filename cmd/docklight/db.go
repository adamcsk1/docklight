package main

import "errors"

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
	if tables > 1 || columns != "id_hash,expires_at" {
		return errors.New("unsupported UI database schema: set DB_PATH to a fresh sessions database (default /data/sessions.db); existing data has not been changed")
	}
	return nil
}
