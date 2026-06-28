package main

import (
	"os"
	"path/filepath"
	"runtime"
)

func ensureDataStorage(cfg Config) error {
	dataDir := filepath.Dir(cfg.DBPath)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	return chownDataPath(cfg, dataDir)
}

func (app *App) chownDataFiles() error {
	for _, path := range []string{app.cfg.DBPath, app.cfg.DBPath + "-wal", app.cfg.DBPath + "-shm"} {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := chownDataPath(app.cfg, path); err != nil {
			return err
		}
	}
	return nil
}

func chownDataPath(cfg Config, targetPath string) error {
	if cfg.DataFileOwner == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		return errWindowsChownUnsupported("DATA_FILE_OWNER", targetPath)
	}
	return chownPath(targetPath, cfg.DataFileOwner.UID, cfg.DataFileOwner.GID)
}
