package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocklightSelfUpdateRunsOwnComposeService(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Fatal("hostname is required for self-update test")
	}
	workDir := filepath.ToSlash(t.TempDir())
	composeFile := filepath.ToSlash(filepath.Join(workDir, "compose.yml"))
	if err := os.WriteFile(composeFile, []byte("services:\n  docklight:\n    image: docklight:local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := dir + "|" + name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch name + " " + strings.Join(args, " ") {
		case "docker container ls -aq":
			return "docklightcontainer\n", nil
		case "docker container inspect docklightcontainer":
			return fmt.Sprintf(`[{"ID":%q,"Name":"/docklight","Image":"sha256:docklightimage","Config":{"Image":"ghcr.io/adamcsk1/docklight:latest","Env":["DOCKER_CONFIG=/root/.docker"],"Labels":{"com.docker.compose.project":"docklight","com.docker.compose.service":"docklight","com.docker.compose.project.working_dir":%q,"com.docker.compose.project.config_files":%q}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"Mounts":[{"Type":"bind","Source":"/var/run/docker.sock","Destination":"/var/run/docker.sock"},{"Type":"bind","Source":%q,"Destination":%q},{"Type":"bind","Source":"/host/docker-config","Destination":"/root/.docker"}]}]`, hostname, workDir, composeFile, workDir, workDir), nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=exited":
			return "oldhelper\n", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=created":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=dead":
			return "", nil
		case "docker container rm oldhelper":
			return "oldhelper", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter label=docklight.self-update.project=docklight":
			return "", nil
		default:
			if strings.HasPrefix(name+" "+strings.Join(args, " "), "docker run -d --name docklight-self-update-docklight ") {
				return "helper123", nil
			}
			return "", nil
		}
	}

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/settings/docklight/update", nil)
	rec := httptest.NewRecorder()
	app.updateDocklight(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected self-update redirect, got %d", rec.Code)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "|docker run -d --rm --name docklight-self-update-docklight ") ||
		!strings.Contains(joined, "-v /var/run/docker.sock:/var/run/docker.sock") ||
		!strings.Contains(joined, "-v "+workDir+":"+workDir+":ro") ||
		!strings.Contains(joined, "-v /host/docker-config:/root/.docker:ro") ||
		!strings.Contains(joined, "'docker' 'compose' '--project-name' 'docklight' '--project-directory' '"+workDir+"' '-f' '"+composeFile+"' 'pull' 'docklight'") ||
		!strings.Contains(joined, "'docker' 'compose' '--project-name' 'docklight' '--project-directory' '"+workDir+"' '-f' '"+composeFile+"' 'up' '-d' '--no-deps' '--force-recreate' 'docklight'") {
		t.Fatalf("expected detached helper docker run with compose pull/up, calls=%s", joined)
	}
}

func TestSelfUpdateAutoRemoveFormValue(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "missing defaults true", body: "", want: true},
		{name: "checked true", body: "auto_remove_helper=0&auto_remove_helper=1", want: true},
		{name: "unchecked false", body: "auto_remove_helper=0", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://docklight.local/settings/docklight/update", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if got := selfUpdateAutoRemove(req); got != tc.want {
				t.Fatalf("expected auto remove %v, got %v", tc.want, got)
			}
		})
	}
}

func TestDocklightSelfUpdateRequiresComposeLabels(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Fatal("hostname is required for self-update test")
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		switch name + " " + strings.Join(args, " ") {
		case "docker container ls -aq":
			return "docklightcontainer\n", nil
		case "docker container inspect docklightcontainer":
			return fmt.Sprintf(`[{"ID":%q,"Name":"/docklight","Image":"sha256:docklightimage","Config":{"Image":"docklight:local","Labels":{"com.docker.compose.project":"docklight","com.docker.compose.service":"docklight"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}]`, hostname), nil
		case "docker compose --project-name docklight --project-directory  pull docklight":
			t.Fatal("self-update should not run without compose working_dir and config_files labels")
		}
		return "", nil
	}

	_, err = app.docklightSelfUpdateTarget()
	if err == nil || !strings.Contains(err.Error(), "requires Docker Compose labels") {
		t.Fatalf("expected missing compose label error, got %v", err)
	}
}

func TestDocklightSelfUpdateHelperUsesDockerConfigParentMount(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	var runCall string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=exited":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=created":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=dead":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter label=docklight.self-update.project=docklight":
			return "", nil
		}
		if name == "docker" && len(args) > 0 && args[0] == "run" {
			runCall = call
			return "helper", nil
		}
		return "", nil
	}
	target := docklightSelfUpdateTarget{Project: "docklight", Service: "docklight", WorkingDir: "/opt/docklight", ConfigFiles: []string{"/opt/docklight/compose.yml"}, Image: "docklight:latest", DockerConfig: "/root/.docker", HelperName: "docklight-self-update-test", Mounts: []dockerMount{{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}, {Source: "/opt", Destination: "/opt"}, {Source: "/host/root", Destination: "/root"}}}
	if _, err := app.launchDocklightSelfUpdateHelper(target); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runCall, "-v /host/root:/root:ro") || strings.Contains(runCall, "-v /host/root:/root/.docker:ro") {
		t.Fatalf("expected parent docker config mount to be preserved, call=%s", runCall)
	}
	if strings.Contains(runCall, " --rm ") {
		t.Fatalf("expected helper auto-remove to be opt-in on direct target launch, call=%s", runCall)
	}
}

func TestDocklightSelfUpdateHelperRejectsConcurrentRun(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		switch name + " " + strings.Join(args, " ") {
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=exited":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=created":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=dead":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter label=docklight.self-update.project=docklight":
			return "runninghelper\n", nil
		}
		if name == "docker" && len(args) > 0 && args[0] == "run" {
			t.Fatal("helper should not start while another helper is running")
		}
		return "", nil
	}
	target := docklightSelfUpdateTarget{Project: "docklight", Service: "docklight", WorkingDir: "/opt/docklight", ConfigFiles: []string{"/opt/docklight/compose.yml"}, Image: "docklight:latest", HelperName: "docklight-self-update-test", Mounts: []dockerMount{{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}, {Source: "/opt", Destination: "/opt"}}}
	_, err := app.launchDocklightSelfUpdateHelper(target)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected running helper error, got %v", err)
	}
}

func TestDocklightSelfUpdateAllowsParentProjectMount(t *testing.T) {
	target := docklightSelfUpdateTarget{WorkingDir: "/opt/docklight", Mounts: []dockerMount{{Source: "/opt", Destination: "/opt"}}}
	if mount, ok := target.mountForPath("/opt/docklight"); !ok || mount.Destination != "/opt" {
		t.Fatalf("expected parent /opt mount to cover /opt/docklight, mount=%#v ok=%v", mount, ok)
	}
}

func TestDocklightSelfUpdateHelperNarrowsParentProjectMount(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	var runCall string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=exited":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=created":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter status=dead":
			return "", nil
		case "docker container ls -aq --filter label=docklight.self-update=true --filter label=docklight.self-update.project=docklight":
			return "", nil
		}
		if name == "docker" && len(args) > 0 && args[0] == "run" {
			runCall = call
			return "helper", nil
		}
		return "", nil
	}
	target := docklightSelfUpdateTarget{Project: "docklight", Service: "docklight", WorkingDir: "/opt/docklight", ConfigFiles: []string{"/opt/docklight/compose.yml"}, Image: "docklight:latest", HelperName: "docklight-self-update-test", Mounts: []dockerMount{{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}, {Source: "/opt", Destination: "/opt"}}}
	if _, err := app.launchDocklightSelfUpdateHelper(target); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runCall, "-v /opt/docklight:/opt/docklight:ro") || strings.Contains(runCall, "-v /opt:/opt:ro") {
		t.Fatalf("expected helper to mount only project directory, call=%s", runCall)
	}
}

func TestDocklightSelfUpdateRejectsDifferentHostProjectPath(t *testing.T) {
	workDir := filepath.ToSlash(t.TempDir())
	composeFile := filepath.Join(workDir, "compose.yml")
	if err := os.WriteFile(composeFile, []byte("services:\n  docklight:\n    image: docklight:local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := docklightSelfUpdateTarget{
		Project:     "docklight",
		Service:     "docklight",
		WorkingDir:  workDir,
		ConfigFiles: []string{composeFile},
		Image:       "docklight:latest",
		Mounts: []dockerMount{
			{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
			{Source: "/host/docklight", Destination: workDir},
		},
	}
	if err := target.validate(); err == nil || !strings.Contains(err.Error(), "same absolute path") {
		t.Fatalf("expected same-path mount validation error, got %v", err)
	}
}

func TestDocklightSelfUpdateHelperEnvFiltersSecrets(t *testing.T) {
	env := selfUpdateHelperEnv([]string{"IMAGE_TAG=latest", "COMPOSE_PROFILES=prod", "ADMIN_PASSWORD=secret", "SESSION_SECRET=secret", "API_TOKEN=secret", "DOCKER_CONFIG=/root/.docker"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "IMAGE_TAG=latest") || !strings.Contains(joined, "COMPOSE_PROFILES=prod") {
		t.Fatalf("expected non-sensitive compose env to pass through, got %v", env)
	}
	for _, blocked := range []string{"ADMIN_PASSWORD", "SESSION_SECRET", "API_TOKEN", "DOCKER_CONFIG"} {
		if strings.Contains(joined, blocked) {
			t.Fatalf("expected %s to be filtered, got %v", blocked, env)
		}
	}
}
