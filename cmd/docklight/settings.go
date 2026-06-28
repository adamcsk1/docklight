package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	defaultActivityRetentionDays   = 30
	defaultVerboseLogRetentionDays = 30
)

var retentionDayOptions = []int{30, 60, 120, 180, 365}

type AppSettings struct {
	ActivityRetentionDays   int
	VerboseLogRetentionDays int
	VerboseLoggingEnabled   bool
}

type appSettingsFile struct {
	ActivityRetentionDays   int  `yaml:"activity_retention_days"`
	VerboseLogRetentionDays int  `yaml:"verbose_log_retention_days"`
	VerboseLoggingEnabled   bool `yaml:"verbose_logging_enabled"`
}

func (app *App) ensureDefaultSettings() error {
	if _, err := os.Stat(app.settingsPath()); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return app.saveSettings(AppSettings{ActivityRetentionDays: defaultActivityRetentionDays, VerboseLogRetentionDays: defaultVerboseLogRetentionDays})
}

func (app *App) loadSettings() (AppSettings, error) {
	settings := AppSettings{ActivityRetentionDays: defaultActivityRetentionDays, VerboseLogRetentionDays: defaultVerboseLogRetentionDays}
	content, err := os.ReadFile(app.settingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	var fileSettings appSettingsFile
	if err := yaml.Unmarshal(content, &fileSettings); err != nil {
		return settings, err
	}
	if validRetentionDays(fileSettings.ActivityRetentionDays) {
		settings.ActivityRetentionDays = fileSettings.ActivityRetentionDays
	}
	if validRetentionDays(fileSettings.VerboseLogRetentionDays) {
		settings.VerboseLogRetentionDays = fileSettings.VerboseLogRetentionDays
	}
	settings.VerboseLoggingEnabled = fileSettings.VerboseLoggingEnabled
	return settings, nil
}

func (app *App) saveSettings(settings AppSettings) error {
	if !validRetentionDays(settings.ActivityRetentionDays) {
		return fmt.Errorf("invalid activity retention: %d", settings.ActivityRetentionDays)
	}
	if !validRetentionDays(settings.VerboseLogRetentionDays) {
		return fmt.Errorf("invalid verbose log retention: %d", settings.VerboseLogRetentionDays)
	}
	if err := os.MkdirAll(dataDir(app.cfg), 0o755); err != nil {
		return err
	}
	if err := chownDataPath(app.cfg, dataDir(app.cfg)); err != nil {
		return err
	}
	content, err := yaml.Marshal(appSettingsFile{
		ActivityRetentionDays:   settings.ActivityRetentionDays,
		VerboseLogRetentionDays: settings.VerboseLogRetentionDays,
		VerboseLoggingEnabled:   settings.VerboseLoggingEnabled,
	})
	if err != nil {
		return err
	}
	path := app.settingsPath()
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0o644); err != nil {
		return err
	}
	if err := chownDataPath(app.cfg, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return chownDataPath(app.cfg, path)
}

func (app *App) settingsPath() string {
	return filepath.Join(dataDir(app.cfg), "config.yml")
}

func validRetentionDays(days int) bool {
	for _, option := range retentionDayOptions {
		if days == option {
			return true
		}
	}
	return false
}
