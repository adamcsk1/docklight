package main

import (
	"database/sql"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed templates/*.html static/app.css static/logo.ico
var webFS embed.FS

type App struct {
	cfg       Config
	db        *sql.DB
	templates *template.Template
	loginMu   sync.Mutex
	logins    map[string]loginAttempt
}
type loginAttempt struct {
	Count     int
	FirstSeen int64
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(cfg.DBPath), 0700); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	app := &App{cfg: cfg, db: db}
	if err = app.initDB(); err != nil {
		log.Fatal(err)
	}
	if err = os.Chmod(cfg.DBPath, 0600); err != nil {
		log.Fatal(err)
	}
	if err = app.loadTemplates(); err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: cfg.Addr, Handler: app.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: time.Minute, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	log.Printf("Docklight listening on %s%s", cfg.Addr, cfg.BaseURL)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
