package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestActivityLogPreservesFullOutputAndOrdersDocklightEntriesLast(t *testing.T) {
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
	if activities[1].Output != longOutput {
		t.Fatalf("expected full activity output to be preserved, len=%d", len(activities[1].Output))
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

func TestActivityStatusReturnsActivityJSON(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	id, err := app.createActivity("web", "upgrade", "running", "pulling")
	if err != nil {
		t.Fatal(err)
	}
	app.updateActivity(id, "success", "done")

	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/activity/1/status", nil)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	app.activityStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected activity status JSON, got %d", rec.Code)
	}
	var activity Activity
	if err := json.Unmarshal(rec.Body.Bytes(), &activity); err != nil {
		t.Fatal(err)
	}
	if activity.ID != id || activity.StackName != "web" || activity.Action != "upgrade" || activity.Status != "success" || activity.Output != "done" {
		t.Fatalf("unexpected activity payload: %#v", activity)
	}
}

func TestRunningOperationPublishesProgressOutput(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	progressed := make(chan struct{})
	release := make(chan struct{})
	id, started, err := app.startStackOperationLocked("web", "upgrade", "upgrade started", func() {}, func(progress operationProgress) (string, error) {
		progress("pulling image layer")
		close(progressed)
		<-release
		return "operation complete", nil
	})
	if err != nil || !started {
		t.Fatalf("expected operation to start, started=%v err=%v", started, err)
	}
	<-progressed

	activity := waitForActivityOutput(t, app, id, "running", "pulling image layer")
	if activity.Output != "pulling image layer" {
		t.Fatalf("expected live progress output, got %#v", activity)
	}
	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/activity/1/status", nil)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	app.activityStatus(rec, req)
	var payload Activity
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || payload.Status != "running" || payload.Output != "pulling image layer" {
		t.Fatalf("expected running progress payload, code=%d payload=%#v", rec.Code, payload)
	}

	close(release)
	activity = waitForActivityStatus(t, app, "web", "upgrade", "success")
	if activity.Output != "operation complete" {
		t.Fatalf("expected final output, got %q", activity.Output)
	}
}

func TestInitDBFailsStaleRunningActivities(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO activity_log (stack_name, action, status, output) VALUES ('web', 'upgrade', 'running', 'started')`); err != nil {
		t.Fatal(err)
	}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	activity := waitForActivityStatus(t, app, "web", "upgrade", "failed")
	if !strings.Contains(activity.Output, "Docklight restarted") {
		t.Fatalf("expected stale operation output, got %q", activity.Output)
	}
}

func TestInitDBLeavesHistoricalStartedActivities(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO activity_log (stack_name, action, status, output) VALUES ('web', 'update stack', 'started', 'stack update started')`); err != nil {
		t.Fatal(err)
	}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	activity := waitForActivityStatus(t, app, "web", "update stack", "started")
	if strings.Contains(activity.Output, "Docklight restarted") {
		t.Fatalf("historical started activity was marked stale: %q", activity.Output)
	}
}

func waitForActivityOutput(t *testing.T, app *App, id int64, wantStatus, wantOutput string) Activity {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		activity, err := app.loadActivityByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if activity.Status == wantStatus && activity.Output == wantOutput {
			return activity
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for activity %d status=%s output=%q", id, wantStatus, wantOutput)
	return Activity{}
}
