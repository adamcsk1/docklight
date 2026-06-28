package main

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	if err := ensureDataStorage(cfg); err != nil {
		logger.Error("failed to create data directory", "error", err)
		os.Exit(1)
	}

	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	app := &App{cfg: cfg, db: db, logger: logger}
	if err := app.initDB(); err != nil {
		logger.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}
	if err := app.chownDataFiles(); err != nil {
		logger.Error("failed to set data file ownership", "error", err)
		os.Exit(1)
	}
	if err := app.purgeExpiredActivity(); err != nil {
		logger.Warn("startup activity purge failed", "error", err)
	}
	if err := app.purgeExpiredVerboseLogs(); err != nil {
		logger.Warn("startup verbose log purge failed", "error", err)
	}
	if err := app.loadTemplates(); err != nil {
		logger.Error("failed to load templates", "error", err)
		os.Exit(1)
	}
	if err := app.rescanStacks(); err != nil {
		logger.Warn("startup rescan failed", "error", err)
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.ActionTimeout + 30*time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	logger.Info("docklight listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "stacks_root", cfg.StacksRoot)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
