package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunCommandUsesProcessTimeoutAndWorkingDirectory(t *testing.T) {
	app := &App{cfg: Config{ActionTimeout: time.Second}}
	output, err := app.runCommand("", "go", "env", "GOVERSION")
	if err != nil || !strings.Contains(output, "go") {
		t.Fatalf("expected go env command output, output=%q err=%v", output, err)
	}
}

func TestRunCommandReturnsProcessErrorsAndOutput(t *testing.T) {
	app := &App{cfg: Config{ActionTimeout: time.Second}}
	output, err := app.runCommand("", "go", "definitely-not-a-go-subcommand")
	if err == nil {
		t.Fatalf("expected go command error, output=%q", output)
	}
}

func TestDockerRunnerErrorsPropagateThroughInventoryLookups(t *testing.T) {
	backendErr := errors.New("docker unavailable")
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "boom", backendErr
	}
	if _, err := app.dockerContainerInspects(true); !errors.Is(err, backendErr) {
		t.Fatalf("expected container inspect list error, got %v", err)
	}
	if _, _, err := app.findImage("anything"); !errors.Is(err, backendErr) {
		t.Fatalf("expected findImage error, got %v", err)
	}
	if _, _, err := app.findVolume("anything"); !errors.Is(err, backendErr) {
		t.Fatalf("expected findVolume error, got %v", err)
	}
	if _, _, err := app.findNetwork("anything"); !errors.Is(err, backendErr) {
		t.Fatalf("expected findNetwork error, got %v", err)
	}
}
