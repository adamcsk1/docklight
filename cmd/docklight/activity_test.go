package main

import (
	"strings"
	"testing"
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
