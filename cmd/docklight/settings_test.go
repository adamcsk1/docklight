package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSettingsDefaultsAndSave(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	settings, err := app.loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.ActivityRetentionDays != 30 || settings.VerboseLogRetentionDays != 30 || settings.VerboseLoggingEnabled {
		t.Fatalf("unexpected default settings: %#v", settings)
	}
	if err := app.saveSettings(AppSettings{ActivityRetentionDays: 120, VerboseLogRetentionDays: 180, VerboseLoggingEnabled: true}); err != nil {
		t.Fatal(err)
	}
	settings, err = app.loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.ActivityRetentionDays != 120 || settings.VerboseLogRetentionDays != 180 || !settings.VerboseLoggingEnabled {
		t.Fatalf("settings were not saved: %#v", settings)
	}
	if err := app.saveSettings(AppSettings{ActivityRetentionDays: 7, VerboseLogRetentionDays: 30}); err == nil {
		t.Fatal("expected invalid retention to fail")
	}
	if err := app.saveSettings(AppSettings{ActivityRetentionDays: 30, VerboseLogRetentionDays: 7}); err == nil {
		t.Fatal("expected invalid verbose log retention to fail")
	}
}

func TestUpdateSettingsHandlerPurgesExpiredActivity(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO activity_log (stack_name, action, status, output, created_at) VALUES ('old', 'test', 'success', '', datetime('now', '-31 days')), ('new', 'test', 'success', '', datetime('now', '-2 days'))`); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"activity_retention_days": {"30"}, "verbose_log_retention_days": {"30"}}
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/settings", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.updateSettings(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM activity_log`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one retained activity row, got %d", count)
	}
}

func TestDownloadActivityExportsAllRows(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	for i := 0; i < 105; i++ {
		if _, err := db.Exec(`INSERT INTO activity_log (stack_name, action, status, output) VALUES (?, 'deploy', 'success', ?)`, "stack", "row"); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	app.downloadActivity(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/activity/download", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "docklight-activity-") {
		t.Fatalf("missing attachment header: %q", rec.Header().Get("Content-Disposition"))
	}
	if got := strings.Count(rec.Body.String(), "stack,deploy,success,row"); got != 105 {
		t.Fatalf("expected 105 exported rows, got %d", got)
	}
}

func TestVerboseLogWritesAndDownloadsWhenEnabled(t *testing.T) {
	dataDir := t.TempDir()
	cfg := Config{BaseURL: "/", StacksRoot: t.TempDir(), DBPath: filepath.Join(dataDir, "docklight.db"), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"}
	var calls *[]chownCall
	if runtime.GOOS != "windows" {
		calls = recordChown(t)
		cfg.DataFileOwner = &fileOwner{UID: 2000, GID: 2001}
	}
	app, _ := newTestApp(t, cfg)
	path := app.verboseLogPath(time.Now())
	app.verboseLog("disabled message")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no log while disabled, stat error: %v", err)
	}
	if err := app.saveSettings(AppSettings{ActivityRetentionDays: 30, VerboseLogRetentionDays: 30, VerboseLoggingEnabled: true}); err != nil {
		t.Fatal(err)
	}
	app.verboseLog("enabled message", "key", "value")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "enabled message key=\"value\"") {
		t.Fatalf("unexpected verbose log content: %s", content)
	}
	if calls != nil {
		assertChownCall(t, *calls, dataDir, 2000, 2001)
		assertChownCall(t, *calls, app.verboseLogDir(), 2000, 2001)
		assertChownCall(t, *calls, path, 2000, 2001)
	}
	rec := httptest.NewRecorder()
	app.downloadVerboseLog(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/settings/verbose-log/download", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), filepath.Base(path)) {
		t.Fatalf("missing verbose log attachment header: %q", rec.Header().Get("Content-Disposition"))
	}
}

func TestVerboseLogHistoricalListDownloadAndRetention(t *testing.T) {
	dataDir := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), DBPath: filepath.Join(dataDir, "docklight.db"), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	logDir := app.verboseLogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldName := "docklight-verbose-2020-01-01.log"
	recentName := "docklight-verbose-2099-01-01.log"
	if err := os.WriteFile(filepath.Join(logDir, oldName), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, recentName), []byte("recent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "not-a-docklight.log"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := app.verboseLogFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != recentName || files[1] != oldName {
		t.Fatalf("unexpected verbose log file list: %#v", files)
	}
	rec := httptest.NewRecorder()
	app.downloadVerboseLog(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/settings/verbose-log/download?file="+recentName, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "recent") {
		t.Fatalf("expected historical log download, code=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	app.downloadVerboseLog(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/settings/verbose-log/download?file=../docklight.db", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for unsafe filename, got %d", rec.Code)
	}
	if err := app.saveSettings(AppSettings{ActivityRetentionDays: 30, VerboseLogRetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err := app.purgeExpiredVerboseLogs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(logDir, oldName)); !os.IsNotExist(err) {
		t.Fatalf("expected old verbose log to be purged, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, recentName)); err != nil {
		t.Fatalf("expected recent verbose log to remain: %v", err)
	}
}

func TestSettingsVerboseLogDownloadUsesFileSelect(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/docklight", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/docklight/settings", nil)
	app.render(rec, req, "settings.html", PageData{Title: "Settings", Config: app.cfg, VerboseLogFiles: []string{"docklight-verbose-2026-06-19.log", "docklight-verbose-2026-06-18.log"}})
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected settings render, got %d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, `method="get" action="/docklight/settings/verbose-log/download"`) || !strings.Contains(body, `select name="file"`) || !strings.Contains(body, `option value="docklight-verbose-2026-06-19.log"`) || !strings.Contains(body, "Download selected log") {
		t.Fatalf("expected verbose log select download form, body=%s", body)
	}
	if !strings.Contains(body, `name="auto_remove_helper" value="0"`) || !strings.Contains(body, `name="auto_remove_helper" value="1" checked`) || !strings.Contains(body, "Remove update helper automatically") {
		t.Fatalf("expected checked update helper auto-remove option, body=%s", body)
	}
	if strings.Contains(body, `?file=docklight-verbose-2026-06-19.log`) {
		t.Fatalf("expected no per-file verbose log download links, body=%s", body)
	}
}

func TestVerboseLogFileNameValidationRejectsUnsafeAndInvalidDates(t *testing.T) {
	valid := "docklight-verbose-2026-06-18.log"
	if !validVerboseLogFileName(valid) {
		t.Fatalf("expected %s to be valid", valid)
	}
	for _, name := range []string{
		"../docklight-verbose-2026-06-18.log",
		`..\docklight-verbose-2026-06-18.log`,
		"docklight-verbose-2026-02-30.log",
		"docklight-verbose-2026-6-18.log",
		"docklight-verbose-2026-06-18.log.bak",
		"other-verbose-2026-06-18.log",
	} {
		if validVerboseLogFileName(name) {
			t.Fatalf("expected %q to be invalid", name)
		}
	}
}
