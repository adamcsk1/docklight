package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
)

func (app *App) initDB() error {
	var version int
	if err := app.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version < 2 {
		if info, err := os.Stat(app.cfg.DBPath); err == nil && info.Size() > 0 {
			backup := app.cfg.DBPath + ".pre-agent.bak"
			if _, err = os.Stat(backup); os.IsNotExist(err) {
				if _, err = app.db.Exec(`VACUUM INTO '` + strings.ReplaceAll(backup, "'", "''") + `'`); err != nil {
					return fmt.Errorf("database backup failed: %w", err)
				}
				if err = os.Chmod(backup, 0600); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		}
	}
	// Keep existing stack tables and activity history. No user configuration is deleted.
	if _, err := app.db.Exec(`
PRAGMA busy_timeout = 5000;
CREATE TABLE IF NOT EXISTS activity_log (
 id INTEGER PRIMARY KEY AUTOINCREMENT, stack_name TEXT NOT NULL, action TEXT NOT NULL,
 status TEXT NOT NULL, output TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);`); err != nil {
		return err
	}
	rows, err := app.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return err
	}
	legacy, hashed := false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue sql.NullString
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		legacy = legacy || name == "id"
		hashed = hashed || name == "id_hash"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if legacy && !hashed {
		if _, err = app.db.Exec(`DROP TABLE sessions`); err != nil {
			return err
		}
	}
	_, err = app.db.Exec(`CREATE TABLE IF NOT EXISTS sessions (id_hash TEXT PRIMARY KEY, username TEXT NOT NULL, expires_at TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP); PRAGMA user_version = 2;`)
	return err
}

func (app *App) logActivity(stack, action, status, output string) {
	_, err := app.db.Exec(`INSERT INTO activity_log(stack_name,action,status,output) VALUES (?,?,?,?)`, stack, action, status, output)
	if err != nil {
		return
	}
	_, _ = app.db.Exec(`DELETE FROM activity_log WHERE created_at < datetime('now','-30 days')`)
}
