package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const verboseLogPrefix = "docklight-verbose-"
const verboseLogSuffix = ".log"

func (app *App) verboseLog(message string, attrs ...string) {
	if app.db == nil {
		return
	}
	settings, err := app.loadSettings()
	if err != nil || !settings.VerboseLoggingEnabled {
		return
	}
	line := time.Now().Format(time.RFC3339) + " " + message
	for i := 0; i+1 < len(attrs); i += 2 {
		line += " " + attrs[i] + "=" + quoteLogValue(attrs[i+1])
	}
	line += "\n"

	app.verboseMu.Lock()
	defer app.verboseMu.Unlock()
	path := app.verboseLogPath(time.Now())
	logDir := filepath.Dir(path)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		app.logger.Warn("failed to create verbose log directory", "error", err)
		return
	}
	if err := chownDataPath(app.cfg, logDir); err != nil {
		app.logger.Warn("failed to set verbose log directory ownership", "error", err)
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		app.logger.Warn("failed to open verbose log", "error", err)
		return
	}
	defer file.Close()
	if err := chownDataPath(app.cfg, path); err != nil {
		app.logger.Warn("failed to set verbose log ownership", "error", err)
		return
	}
	if _, err := file.WriteString(line); err != nil {
		app.logger.Warn("failed to write verbose log", "error", err)
	}
	if err := app.purgeExpiredVerboseLogs(); err != nil {
		app.logger.Warn("failed to purge verbose logs", "error", err)
	}
}

func (app *App) verboseLogPath(t time.Time) string {
	return filepath.Join(app.verboseLogDir(), verboseLogFileName(t))
}

func (app *App) verboseLogDir() string {
	return filepath.Join(dataDir(app.cfg), "logs")
}

func verboseLogFileName(t time.Time) string {
	return verboseLogPrefix + t.Format("2006-01-02") + verboseLogSuffix
}

func (app *App) verboseLogFiles() ([]string, error) {
	entries, err := os.ReadDir(app.verboseLogDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if validVerboseLogFileName(name) {
			files = append(files, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files, nil
}

func validVerboseLogFileName(name string) bool {
	if filepath.Base(name) != name || len(name) != len(verboseLogPrefix)+len("2006-01-02")+len(verboseLogSuffix) {
		return false
	}
	if !strings.HasPrefix(name, verboseLogPrefix) || !strings.HasSuffix(name, verboseLogSuffix) {
		return false
	}
	datePart := strings.TrimSuffix(strings.TrimPrefix(name, verboseLogPrefix), verboseLogSuffix)
	parsed, err := time.Parse("2006-01-02", datePart)
	return err == nil && parsed.Format("2006-01-02") == datePart
}

func verboseLogFileDate(name string) (time.Time, bool) {
	if !validVerboseLogFileName(name) {
		return time.Time{}, false
	}
	datePart := strings.TrimSuffix(strings.TrimPrefix(name, verboseLogPrefix), verboseLogSuffix)
	parsed, err := time.Parse("2006-01-02", datePart)
	return parsed, err == nil
}

func (app *App) purgeExpiredVerboseLogs() error {
	settings, err := app.loadSettings()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(app.verboseLogDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -settings.VerboseLogRetentionDays)
	cutoffDate := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		logDate, ok := verboseLogFileDate(name)
		if !ok || !logDate.Before(cutoffDate) {
			continue
		}
		if err := os.Remove(filepath.Join(app.verboseLogDir(), name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func quoteLogValue(value string) string {
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, "\r", `\r`)
	return fmt.Sprintf("%q", value)
}

func (app *App) withVerboseRequestLog(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		handler.ServeHTTP(recorder, r)
		app.verboseLog("http request", "method", r.Method, "path", r.URL.Path, "status", fmt.Sprintf("%d", recorder.status), "duration", time.Since(start).String())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (app *App) downloadVerboseLog(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("file"))
	if name == "" {
		name = verboseLogFileName(time.Now())
	}
	if !validVerboseLogFileName(name) {
		http.Error(w, "invalid verbose log file", http.StatusBadRequest)
		return
	}
	path := filepath.Join(app.verboseLogDir(), name)
	if _, err := os.Stat(path); err != nil {
		http.Error(w, "verbose log not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}
