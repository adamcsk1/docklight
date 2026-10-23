package management

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Explicit opt-in must point at a disposable Docker-in-Docker daemon, never a real host.
func TestDockerIntegration(t *testing.T) {
	host := os.Getenv("DOCKLIGHT_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set DOCKLIGHT_TEST_DOCKER_HOST to disposable DinD daemon")
	}
	ctx := context.Background()
	run := func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		cmd := exec.CommandContext(ctx, "docker", append([]string{"--host", host}, args...)...)
		// Compose's Buildx subprocess must use the same isolated daemon as the CLI.
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+host, "DOCKER_CONTEXT=", "BUILDX_BUILDER=default")
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = out, out
		return cmd.Run()
	}
	docker := func(args ...string) string {
		t.Helper()
		var b bytes.Buffer
		if err := run(ctx, "", &b, args...); err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, b.String())
		}
		return strings.TrimSpace(b.String())
	}
	// Verify this endpoint was deliberately created for these tests.
	if got := docker("info", "--format", "{{.Name}}"); got != "docklight-isolated-test" {
		t.Fatalf("refusing destructive tests against non-test daemon %q; start DinD with --hostname docklight-isolated-test", got)
	}
	root := t.TempDir()
	file := filepath.Join(root, "compose.yml")
	content := "services:\n  web:\n    image: docklight-test-image:mutable\n    command: [sh, -c, 'sleep 3600']\n    environment:\n      DEPLOYMENT: ${DEPLOYMENT:-wrong}\n    volumes: [data:/data]\nvolumes:\n  data: {}\n"
	envFile := filepath.Join(root, "production.env")
	if err := os.WriteFile(envFile, []byte("DEPLOYMENT=production\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	docker("pull", "alpine:3.23")
	docker("tag", "alpine:3.23", "docklight-test-image:mutable")
	project := "docklight-integration"
	docker("compose", "-p", project, "-f", file, "--env-file", envFile, "up", "-d")
	t.Cleanup(func() {
		var out bytes.Buffer
		_ = run(ctx, root, &out, "compose", "-p", project, "-f", file, "down", "-v")
	})
	id := docker("compose", "-p", project, "-f", file, "ps", "-q", "web")
	e := &Engine{Root: root, DataDir: t.TempDir(), Run: run}
	original := docker("inspect", "--format", "{{.Image}}", id)
	// Move the mutable tag to a genuinely different image without changing the container.
	docker("pull", "busybox:1.37")
	docker("tag", "busybox:1.37", "docklight-test-image:mutable")
	var output bytes.Buffer
	if err := e.Execute(ctx, Action{Kind: "container", ID: id, Operation: "recreate", Scope: "service"}, &output); err != nil {
		t.Fatalf("recreate: %v\n%s", err, output.String())
	}
	newID := docker("compose", "-p", project, "-f", file, "ps", "-q", "web")
	if newID == id {
		t.Fatal("container not recreated")
	}
	if got := docker("inspect", "--format", "{{.Image}}", newID); got != original {
		t.Fatalf("image changed: %s -> %s", original, got)
	}
	if got := docker("inspect", "--format", "{{json .Config.Env}}", newID); !strings.Contains(got, "DEPLOYMENT=production") {
		t.Fatalf("custom environment lost after recreate: %s", got)
	}
	// The transient override must not break the next management operation.
	inv, err := e.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range inv.Containers {
		if c.ID == newID && !c.ComposeAvailable {
			t.Fatal("recreated Compose target unavailable")
		}
	}
	if err = e.Execute(ctx, Action{Kind: "container", ID: newID, Operation: "upgrade", Scope: "service"}, &output); err == nil {
		t.Fatal("nonexistent registry image unexpectedly pulled")
	}
	if current := docker("compose", "-p", project, "-f", file, "ps", "-q", "web"); current != newID {
		t.Fatal("failed pull replaced container")
	}
	// A successful upgrade uses the original Compose file, not the recreate override.
	if err = os.WriteFile(file, []byte(strings.ReplaceAll(content, "docklight-test-image:mutable", "busybox:1.37")), 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.Execute(ctx, Action{Kind: "container", ID: newID, Operation: "upgrade", Scope: "service"}, &output); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, output.String())
	}
	newID = docker("compose", "-p", project, "-f", file, "ps", "-q", "web")
	if got := docker("inspect", "--format", "{{.Image}}", newID); got == original {
		t.Fatal("upgrade kept old image")
	}
	if err = e.Execute(ctx, Action{Kind: "container", ID: newID, Operation: "stop"}, &output); err != nil {
		t.Fatal(err)
	}
	inv, err = e.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var volumeID string
	for _, r := range inv.Resources {
		if r.Kind == "volume" && strings.Contains(r.Name, project) {
			volumeID = r.ID
		}
	}
	if volumeID == "" {
		b, _ := json.Marshal(inv)
		t.Fatalf("missing volume: %s", b)
	}
	if err = e.Execute(ctx, Action{Kind: "volume", ID: volumeID, Operation: "delete", Confirmed: true}, &output); err == nil {
		t.Fatal("deleted stopped-container volume")
	}
	if err = e.Execute(ctx, Action{Kind: "container", ID: newID, Operation: "delete", Confirmed: true}, &output); err != nil {
		t.Fatal(err)
	}
	docker("volume", "inspect", volumeID)
	if err = e.Execute(ctx, Action{Kind: "volume", ID: volumeID, Operation: "delete", Confirmed: true}, &output); err != nil {
		t.Fatalf("unused volume: %v", err)
	}
	// Compile first; failed builds must preserve the running container.
	content = "services:\n  web:\n    image: docklight-local-test:latest\n    build: .\n    command: [sh, -c, 'sleep 3600']\n"
	if err = os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(root, "Dockerfile")
	writeBuild := func(version string) {
		t.Helper()
		if err := os.WriteFile(dockerfile, []byte("FROM alpine:3.23\nLABEL test.version="+version+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeBuild("one")
	docker("compose", "-p", project, "-f", file, "up", "-d", "--build")
	id = docker("compose", "-p", project, "-f", file, "ps", "-q", "web")
	original = docker("inspect", "--format", "{{.Image}}", id)
	inv, err = e.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range inv.Containers {
		if c.ID == id {
			found = c.Buildable
		}
	}
	if !found {
		t.Fatal("local service not discovered as buildable")
	}
	writeBuild("two")
	if err = e.Execute(ctx, Action{Kind: "container", ID: id, Operation: "build", Scope: "service"}, &output); err != nil {
		t.Fatalf("build: %v\n%s", err, output.String())
	}
	newID = docker("compose", "-p", project, "-f", file, "ps", "-q", "web")
	if newID == id || docker("inspect", "--format", "{{.Image}}", newID) == original {
		t.Fatal("build did not deploy new image")
	}
	if err = os.WriteFile(dockerfile, []byte("FROM alpine:3.23\nRUN exit 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.Execute(ctx, Action{Kind: "container", ID: newID, Operation: "build", Scope: "service"}, &output); err == nil {
		t.Fatal("broken build succeeded")
	}
	if got := docker("compose", "-p", project, "-f", file, "ps", "-q", "web"); got != newID {
		t.Fatal("failed build replaced running container")
	}
}
