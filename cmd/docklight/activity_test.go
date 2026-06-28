package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActivityLogTruncatesAndOrdersDocklightEntriesLast(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	longOutput := strings.Repeat("x", 13000)
	app.logActivity("docklight", "self", "success", longOutput)
	app.logActivity("web", "deploy", "failed", "boom")

	activities, err := app.loadActivity()
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 2 || activities[0].StackName != "web" || activities[1].StackName != "docklight" {
		t.Fatalf("expected non-docklight activity first and docklight last, got %#v", activities)
	}
	if len(activities[1].Output) <= 12000 || !strings.Contains(activities[1].Output, "output truncated") {
		t.Fatalf("expected long activity output to be truncated, len=%d output=%q", len(activities[1].Output), activities[1].Output)
	}
}

func TestLogActivityIgnoresDatabaseWriteFailures(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	app.logActivity("web", "deploy", "failed", "boom")
}

func TestLogActivityWritesVerboseSummaryWhenEnabled(t *testing.T) {
	dataDir := t.TempDir()
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), DBPath: filepath.Join(dataDir, "docklight.db"), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.saveSettings(AppSettings{ActivityRetentionDays: 30, VerboseLogRetentionDays: 30, VerboseLoggingEnabled: true}); err != nil {
		t.Fatal(err)
	}

	app.logActivity("web", "deploy", "failed", "boom")

	content, err := os.ReadFile(app.verboseLogPath(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	logText := string(content)
	for _, want := range []string{`activity recorded`, `stack="web"`, `action="deploy"`, `status="failed"`, `output_bytes="4"`} {
		if !strings.Contains(logText, want) {
			t.Fatalf("expected verbose log to contain %q, got %s", want, logText)
		}
	}
}
