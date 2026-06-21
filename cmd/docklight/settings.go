package main

import (
	"fmt"
	"strconv"
)

const (
	settingActivityRetentionDays   = "activity_retention_days"
	settingVerboseLogRetentionDays = "verbose_log_retention_days"
	settingVerboseLoggingEnabled   = "verbose_logging_enabled"
	defaultActivityRetentionDays   = 30
	defaultVerboseLogRetentionDays = 30
)

var retentionDayOptions = []int{30, 60, 120, 180, 365}

type AppSettings struct {
	ActivityRetentionDays   int
	VerboseLogRetentionDays int
	VerboseLoggingEnabled   bool
}

func (app *App) ensureDefaultSettings() error {
	for key, value := range map[string]string{
		settingActivityRetentionDays:   strconv.Itoa(defaultActivityRetentionDays),
		settingVerboseLogRetentionDays: strconv.Itoa(defaultVerboseLogRetentionDays),
		settingVerboseLoggingEnabled:   "false",
	} {
		if _, err := app.db.Exec(`INSERT OR IGNORE INTO app_settings (key, value) VALUES (?, ?)`, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (app *App) loadSettings() (AppSettings, error) {
	settings := AppSettings{ActivityRetentionDays: defaultActivityRetentionDays, VerboseLogRetentionDays: defaultVerboseLogRetentionDays}
	rows, err := app.db.Query(`SELECT key, value FROM app_settings`)
	if err != nil {
		return settings, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return settings, err
		}
		switch key {
		case settingActivityRetentionDays:
			if days, err := strconv.Atoi(value); err == nil && validRetentionDays(days) {
				settings.ActivityRetentionDays = days
			}
		case settingVerboseLogRetentionDays:
			if days, err := strconv.Atoi(value); err == nil && validRetentionDays(days) {
				settings.VerboseLogRetentionDays = days
			}
		case settingVerboseLoggingEnabled:
			settings.VerboseLoggingEnabled = value == "true"
		}
	}
	return settings, rows.Err()
}

func (app *App) saveSettings(settings AppSettings) error {
	if !validRetentionDays(settings.ActivityRetentionDays) {
		return fmt.Errorf("invalid activity retention: %d", settings.ActivityRetentionDays)
	}
	if !validRetentionDays(settings.VerboseLogRetentionDays) {
		return fmt.Errorf("invalid verbose log retention: %d", settings.VerboseLogRetentionDays)
	}
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	values := map[string]string{
		settingActivityRetentionDays:   strconv.Itoa(settings.ActivityRetentionDays),
		settingVerboseLogRetentionDays: strconv.Itoa(settings.VerboseLogRetentionDays),
		settingVerboseLoggingEnabled:   strconv.FormatBool(settings.VerboseLoggingEnabled),
	}
	for key, value := range values {
		if _, err := tx.Exec(`INSERT INTO app_settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func validRetentionDays(days int) bool {
	for _, option := range retentionDayOptions {
		if days == option {
			return true
		}
	}
	return false
}
