package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunComposeRewritesRelativeBindsWhenHostRootConfigured(t *testing.T) {
	root := t.TempDir()
	hostRoot := filepath.Join(t.TempDir(), "host-stacks")
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "compose.yml"), []byte("services:\n  app:\n    image: nginx\n    volumes:\n      - ./data:/data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, StacksHostRoot: hostRoot, ActionTimeout: time.Minute}}

	var gotDir string
	var gotArgs []string
	var rewritten string
	app.runner = func(dir, name string, args ...string) (string, error) {
		gotDir = dir
		gotArgs = append([]string{name}, args...)
		for i, arg := range args {
			if arg == "-f" && i+1 < len(args) {
				content, err := os.ReadFile(args[i+1])
				if err != nil {
					t.Fatal(err)
				}
				rewritten = string(content)
			}
		}
		return "ok", nil
	}

	stack := Stack{Name: "web", RelativePath: "web", AbsolutePath: stackDir, ComposeFile: "compose.yml"}
	if _, err := app.runCompose(stack, "up", "-d"); err != nil {
		t.Fatal(err)
	}

	wantBindSource := filepath.ToSlash(filepath.Join(hostRoot, "web", "data"))
	if gotDir != stack.AbsolutePath {
		t.Fatalf("expected command dir %q, got %q", stack.AbsolutePath, gotDir)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--project-name web") || !strings.Contains(joined, "--project-directory "+stackDir) {
		t.Fatalf("expected pinned project name and container project dir, got %q", joined)
	}
	if !strings.Contains(rewritten, wantBindSource+":/data") {
		t.Fatalf("expected rewritten bind source %q in compose:\n%s", wantBindSource, rewritten)
	}
}

func TestRunComposeRewritesResolvedEnvBindWhenHostRootConfigured(t *testing.T) {
	root := t.TempDir()
	hostRoot := filepath.Join(t.TempDir(), "host-stacks")
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "compose.yml"), []byte("services:\n  app:\n    image: nginx\n    volumes:\n      - ${DATA_DIR}:/data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, StacksHostRoot: hostRoot, ActionTimeout: time.Minute}}

	var rewritten string
	app.runner = func(dir, name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasSuffix(joined, " config") {
			return fmt.Sprintf("services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: %s\n        target: /data\n", filepath.ToSlash(filepath.Join(stackDir, "data"))), nil
		}
		for i, arg := range args {
			if arg == "-f" && i+1 < len(args) {
				content, err := os.ReadFile(args[i+1])
				if err != nil {
					t.Fatal(err)
				}
				rewritten = string(content)
			}
		}
		return "ok", nil
	}

	stack := Stack{Name: "web", RelativePath: "web", AbsolutePath: stackDir, ComposeFile: "compose.yml"}
	if _, err := app.runCompose(stack, "up", "-d"); err != nil {
		t.Fatal(err)
	}
	want := filepath.ToSlash(filepath.Join(hostRoot, "web", "data"))
	if !strings.Contains(rewritten, want) {
		t.Fatalf("expected resolved bind source %q in compose:\n%s", want, rewritten)
	}
}

func TestRunComposeRewritesResolvedStackRootBindWhenHostRootConfigured(t *testing.T) {
	root := t.TempDir()
	hostRoot := filepath.Join(t.TempDir(), "host-stacks")
	stackDir := filepath.Join(root, "web")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "compose.yml"), []byte("services:\n  app:\n    image: nginx\n    volumes:\n      - ${DATA_DIR}:/data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{StacksRoot: root, StacksHostRoot: hostRoot, ActionTimeout: time.Minute}}

	var rewritten string
	app.runner = func(dir, name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasSuffix(joined, " config") {
			return fmt.Sprintf("services:\n  app:\n    image: nginx\n    volumes:\n      - type: bind\n        source: %s\n        target: /data\n", filepath.ToSlash(stackDir)), nil
		}
		for i, arg := range args {
			if arg == "-f" && i+1 < len(args) {
				content, err := os.ReadFile(args[i+1])
				if err != nil {
					t.Fatal(err)
				}
				rewritten = string(content)
			}
		}
		return "ok", nil
	}

	stack := Stack{Name: "web", RelativePath: "web", AbsolutePath: stackDir, ComposeFile: "compose.yml"}
	if _, err := app.runCompose(stack, "up", "-d"); err != nil {
		t.Fatal(err)
	}
	want := filepath.ToSlash(filepath.Join(hostRoot, "web"))
	if !strings.Contains(rewritten, want) {
		t.Fatalf("expected resolved stack root bind source %q in compose:\n%s", want, rewritten)
	}
}

func TestComposeContainersParsesJSONAndSortsDocklightLast(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	app.runner = func(dir, name string, args ...string) (string, error) {
		if dir != stackDir || name != "docker" || !strings.Contains(strings.Join(args, " "), "ps -a --format json") {
			t.Fatalf("unexpected compose command dir=%q name=%q args=%v", dir, name, args)
		}
		return `[
{"Name":"docklight","Service":"docklight","State":"running","Status":"Up","Image":"docklight:local"},
{"Name":"worker","Service":"job","State":"exited","Status":"Exited","Image":"worker:latest"},
{"Name":"api","Service":"web","State":"running","Status":"Up","Image":"nginx"}
]`, nil
	}

	containers, err := app.composeContainers(Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "compose.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 3 || containers[0].Name != "api" || containers[1].Name != "worker" || containers[2].Name != "docklight" {
		t.Fatalf("expected alphabetical containers with docklight last, got %#v", containers)
	}
	if containers[1].State != "exited" || containers[1].Status != "Exited" || containers[1].Image != "worker:latest" {
		t.Fatalf("expected JSON compose row to be parsed, got %#v", containers[1])
	}
}

func TestComposeContainersParsesJSONLines(t *testing.T) {
	containers, err := parseComposePSJSON(`{"Name":"web-1","Service":"web","State":"running","Status":"Up","Image":"nginx"}
{"Name":"db-1","Service":"db","State":"running","Status":"Up","Image":"postgres"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 2 || containers[0].Name != "web-1" || containers[1].Service != "db" {
		t.Fatalf("expected JSON lines to parse, got %#v", containers)
	}
}

func TestComposeContainersFallsBackToLegacyFormat(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "web")
	app := &App{cfg: Config{StacksRoot: root, ActionTimeout: time.Minute}}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if strings.Contains(call, "ps -a --format json") {
			return "unknown format", errors.New("unknown format")
		}
		return "worker|job|exited\napi|web|running|Up|nginx\n", nil
	}

	containers, err := app.composeContainers(Stack{Name: "web", AbsolutePath: stackDir, ComposeFile: "compose.yml"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "ps -a --format json") || !strings.Contains(joined, "ps --format") {
		t.Fatalf("expected JSON command then legacy fallback, calls:\n%s", joined)
	}
	if len(containers) != 2 || containers[0].Name != "api" || containers[1].Status != "" {
		t.Fatalf("expected padded legacy rows, got %#v", containers)
	}
}
