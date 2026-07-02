package main

import (
	"database/sql"
	"html/template"
	"log/slog"
	"sync"
)

type App struct {
	cfg         Config
	db          *sql.DB
	templates   *template.Template
	logger      *slog.Logger
	runner      commandRunner
	locksMu     sync.Mutex
	locks       map[string]*sync.Mutex
	operationMu sync.RWMutex
	loginMu     sync.Mutex
	logins      map[string]loginAttempt
	verboseMu   sync.Mutex
}

type commandRunner func(dir, name string, args ...string) (string, error)

type loginAttempt struct {
	Count     int
	FirstSeen int64
}
