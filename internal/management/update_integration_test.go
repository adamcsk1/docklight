package management

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Run only against the explicitly named disposable DinD container and daemon.
func TestDockerManagementUpdateIntegration(t *testing.T) {
	host, outer := os.Getenv("DOCKLIGHT_TEST_DOCKER_HOST"), os.Getenv("DOCKLIGHT_TEST_OUTER_CONTAINER")
	if host == "" || outer == "" {
		t.Skip("requires isolated DinD plus prebuilt UI and agent images")
	}
	local := func(args ...string) (string, error) {
		b, err := exec.Command("docker", args...).CombinedOutput()
		return strings.TrimSpace(string(b)), err
	}
	remote := func(args ...string) (string, error) { return local(append([]string{"--host", host}, args...)...) }
	must := func(args ...string) string {
		t.Helper()
		b, err := remote(args...)
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, b)
		}
		return b
	}
	if got := must("info", "--format", "{{.Name}}"); got != "docklight-isolated-test" {
		t.Fatal("refusing non-test daemon")
	}
	if got, err := local("inspect", "--format", "{{.Config.Hostname}}", outer); err != nil || got != "docklight-isolated-test" {
		t.Fatal("refusing non-test outer container", got, err)
	}
	for _, image := range []string{envTest("DOCKLIGHT_TEST_UI_IMAGE", "docklight:refactor"), envTest("DOCKLIGHT_TEST_AGENT_IMAGE", "docklight-agent:refactor")} {
		// Stream binary image archives directly; shell pipes can corrupt them on Windows.
		save := exec.Command("docker", "image", "save", image)
		load := exec.Command("docker", "--host", host, "image", "load")
		pipe, err := save.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		load.Stdin = pipe
		var saveOutput, loadOutput bytes.Buffer
		save.Stderr = &saveOutput
		load.Stdout = &loadOutput
		load.Stderr = &loadOutput
		if err = save.Start(); err != nil {
			t.Fatal(err)
		}
		loadErr := load.Run()
		saveErr := save.Wait()
		if loadErr != nil || saveErr != nil {
			t.Fatalf("load images: %v / %v: %s %s", loadErr, saveErr, saveOutput.String(), loadOutput.String())
		}
	}
	must("run", "-d", "--name", "docklight-e2e-registry", "-p", "5000:5000", "registry:2")
	t.Cleanup(func() { _, _ = remote("rm", "-fv", "docklight-e2e-registry") })
	root := "/srv/docklight-e2e"
	if b, err := local("exec", outer, "mkdir", "-p", root); err != nil {
		t.Fatal(b, err)
	}
	copyFile := func(name, content string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if b, err := local("cp", path, outer+":"+root+"/"+name); err != nil {
			t.Fatal(b, err)
		}
	}
	uiImage, agentImage := envTest("DOCKLIGHT_TEST_UI_IMAGE", "docklight:refactor"), envTest("DOCKLIGHT_TEST_AGENT_IMAGE", "docklight-agent:refactor")
	for _, pair := range [][2]string{{uiImage, "localhost:5000/docklight-ui:latest"}, {agentImage, "localhost:5000/docklight-agent:latest"}} {
		must("tag", pair[0], pair[1])
		must("push", pair[1])
	}
	copyFile("hosts.yml", "hosts:\n  - id: local\n    name: Local\n    url: http://docklight-e2e-agent:8081\n    allow_http: true\n    token: "+testToken+"\n")
	copyFile("hosts-secondary.yml", "hosts:\n  - id: local\n    name: Local\n    url: http://docklight-e2e-agent:8081\n    allow_http: true\n    token: "+testToken+"\n")
	compose := fmt.Sprintf(`services:
  ui:
    image: localhost:5000/docklight-ui:latest
    container_name: docklight-e2e-ui
    labels: {io.docklight.role: ui}
    environment:
      ADMIN_PASSWORD: test-password
      SESSION_SECRET: %s
    volumes:
      - ui-data:/data
      - %s/hosts.yml:/data/hosts.yml:ro
  other-ui:
    image: localhost:5000/docklight-ui:latest
    container_name: docklight-e2e-other-ui
    labels: {io.docklight.role: ui}
    environment:
      ADMIN_PASSWORD: test-password
      SESSION_SECRET: %s
      ADDR: :9090
      BASE_URL: docklight/
    volumes:
      - other-ui-data:/data
      - %s/hosts-secondary.yml:/data/hosts.yml:ro
  agent:
    image: localhost:5000/docklight-agent:latest
    container_name: docklight-e2e-agent
    labels: {io.docklight.role: agent}
    environment:
      AGENT_TOKEN: %s
      STACKS_ROOT: %s
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - %s:%s:ro
      - agent-data:/data
volumes:
  ui-data: {}
  other-ui-data: {}
  agent-data: {}
`, strings.Repeat("s", 32), root, strings.Repeat("s", 32), root, testToken, root, root, root)
	copyFile("compose.yml", compose)
	composeOuter := []string{"exec", outer, "docker", "compose", "-p", "docklight-e2e", "-f", root + "/compose.yml"}
	if b, err := local(append(composeOuter, "up", "-d")...); err != nil {
		t.Fatal(b, err)
	}
	t.Cleanup(func() { _, _ = local(append(composeOuter, "down", "-v")...) })
	request := func(path string, body string) (string, error) {
		args := []string{"exec", "docklight-e2e-agent", "wget", "-qO-", "--header", "Authorization: Bearer " + testToken}
		if body != "" {
			args = append(args, "--post-data", body)
		}
		return remote(append(args, "http://127.0.0.1:8081"+path)...)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if body, err := request("/v1/inventory", ""); err == nil {
			if expected := os.Getenv("DOCKLIGHT_TEST_BUILD_COMMIT"); expected != "" {
				var inv Inventory
				if err := json.Unmarshal([]byte(body), &inv); err != nil || inv.Commit != expected {
					t.Fatalf("agent image build commit: got %q, want %q (%v)", inv.Commit, expected, err)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not start")
		}
		time.Sleep(200 * time.Millisecond)
	}
	waitHealthy := func(t *testing.T, inspect func(...string) string, names ...string) {
		t.Helper()
		statuses := map[string]string{}
		deadline := time.Now().Add(90 * time.Second)
		for {
			healthy := true
			for _, name := range names {
				statuses[name] = inspect("inspect", "--format", "{{.State.Health.Status}}", name)
				healthy = healthy && statuses[name] == "healthy"
			}
			if healthy {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("containers did not become healthy: %v", statuses)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	t.Run("image-health-checks", func(t *testing.T) {
		type healthConfig struct {
			Test                           []string
			Interval, Timeout, StartPeriod time.Duration
			Retries                        int
		}
		configs := map[string]healthConfig{}
		names := []string{"docklight-e2e-ui", "docklight-e2e-other-ui", "docklight-e2e-agent"}
		for _, name := range names {
			var config healthConfig
			raw := must("inspect", "--format", "{{json .Config.Healthcheck}}", name)
			if err := json.Unmarshal([]byte(raw), &config); err != nil {
				t.Fatalf("%s health config: %v (%s)", name, err, raw)
			}
			if len(config.Test) != 2 || config.Test[0] != "CMD-SHELL" || config.Interval != 30*time.Second || config.Timeout != 5*time.Second || config.StartPeriod != 20*time.Second || config.Retries != 3 {
				t.Fatalf("%s inherited wrong health config: %+v", name, config)
			}
			configs[name] = config
		}
		waitHealthy(t, must, names...)
		for _, target := range []struct{ name, url string }{
			{"docklight-e2e-agent", "http://127.0.0.1:8081/healthz"},
			{"docklight-e2e-ui", "http://docklight-e2e-ui:8080/healthz"},
			{"docklight-e2e-other-ui", "http://docklight-e2e-other-ui:9090/docklight/healthz"},
		} {
			body, err := remote("exec", "docklight-e2e-agent", "wget", "-qO-", target.url)
			if err != nil || body != "ok" {
				t.Fatalf("%s unauthenticated health response: %q %v", target.name, body, err)
			}
		}
		if body, err := remote("exec", "docklight-e2e-agent", "wget", "-qO-", "http://127.0.0.1:8081/v1/inventory"); err == nil {
			t.Fatalf("inventory allowed unauthenticated access: %s", body)
		}
		for _, name := range names {
			if body, err := remote("exec", name, "sh", "-c", configs[name].Test[1]); err != nil {
				t.Fatalf("%s runtime probe failed: %s %v", name, body, err)
			}
		}
		for _, test := range []struct {
			name string
			env  []string
		}{
			{"docklight-e2e-ui", []string{"BASE_URL=/"}},
			{"docklight-e2e-ui", []string{"ADDR=   "}},
			{"docklight-e2e-other-ui", []string{"ADDR=127.0.0.1:9090", "BASE_URL=/docklight"}},
			{"docklight-e2e-agent", []string{"ADDR=127.0.0.1:8081"}},
		} {
			args := []string{"exec"}
			for _, value := range test.env {
				args = append(args, "-e", value)
			}
			args = append(args, test.name, "sh", "-c", configs[test.name].Test[1])
			if body, err := remote(args...); err != nil {
				t.Fatalf("%s explicit address/base probe failed: %s %v", test.name, body, err)
			}
		}
	})
	for _, service := range []string{"ui", "agent"} {
		t.Run(service, func(t *testing.T) {
			name := "docklight-e2e-" + service
			id := must("inspect", "--format", "{{.Id}}", name)
			base := uiImage
			if service == "agent" {
				base = agentImage
			}
			copyFile("Dockerfile", "FROM "+base+"\nLABEL docklight.integration.revision=updated\n")
			newTag := "localhost:5000/docklight-" + service + ":latest"
			if b, err := local("exec", outer, "docker", "build", "-t", newTag, root); err != nil {
				t.Fatal(b, err)
			}
			must("push", newTag)
			action, _ := json.Marshal(Action{Kind: "container", ID: id, Operation: "update", Confirmed: true})
			body, err := request("/v1/jobs", string(action))
			if err != nil {
				t.Fatal(body, err)
			}
			var job Job
			if err = json.Unmarshal([]byte(body), &job); err != nil {
				t.Fatal(body, err)
			}
			deadline := time.Now().Add(90 * time.Second)
			for time.Now().Before(deadline) {
				body, err = request("/v1/jobs/"+job.ID, "")
				if err == nil && json.Unmarshal([]byte(body), &job) == nil && (job.Status == "success" || job.Status == "failed") {
					break
				}
				time.Sleep(300 * time.Millisecond)
			}
			if job.Status != "success" {
				t.Fatalf("update did not succeed: %#v\n%s", job, body)
			}
			if after := must("inspect", "--format", "{{.Id}}", name); after == id {
				t.Fatal("management container was not replaced")
			}
			if service == "agent" {
				if _, err = request("/v1/inventory", ""); err != nil {
					t.Fatal("replacement agent unavailable", err)
				}
			}
		})
	}
	t.Run("coordinated-maintenance", func(t *testing.T) {
		// A second disposable Docker daemon exercises real remote-agent/UI replacement.
		must("network", "connect", "docklight-e2e_default", "docklight-e2e-registry")
		must("run", "-d", "--privileged", "--name", "docklight-e2e-remote", "--network", "docklight-e2e_default", "-e", "DOCKER_TLS_CERTDIR=", "docker:29-dind", "--insecure-registry", "docklight-e2e-registry:5000")
		t.Cleanup(func() { _, _ = remote("rm", "-fv", "docklight-e2e-remote") })
		remoteDocker := func(args ...string) (string, error) {
			return remote(append([]string{"exec", "docklight-e2e-remote", "docker"}, args...)...)
		}
		remoteMust := func(args ...string) string {
			t.Helper()
			b, err := remoteDocker(args...)
			if err != nil {
				t.Fatalf("remote Docker %v: %v %s", args, err, b)
			}
			return b
		}
		deadline := time.Now().Add(time.Minute)
		for {
			if _, err := remoteDocker("info"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("remote disposable daemon did not start")
			}
			time.Sleep(300 * time.Millisecond)
		}
		remoteRoot := "/srv/docklight-remote"
		must("exec", "docklight-e2e-remote", "mkdir", "-p", remoteRoot)
		remoteCopy := func(name, content string) {
			t.Helper()
			file := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(file, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			must("cp", file, "docklight-e2e-remote:"+remoteRoot+"/"+name)
		}
		remoteCopy("hosts.yml", "hosts:\n  - id: remote\n    name: Remote\n    url: http://agent:8081\n    allow_http: true\n    token: "+testToken+"\n")
		remoteCompose := fmt.Sprintf(`services:
  agent:
    image: docklight-e2e-registry:5000/docklight-agent:latest
    labels: {io.docklight.role: agent}
    ports: ["8081:8081"]
    environment: {AGENT_TOKEN: %s, STACKS_ROOT: %s}
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - %s:%s:ro
      - agent-data:/data
  ui:
    image: docklight-e2e-registry:5000/docklight-ui:latest
    labels: {io.docklight.role: ui}
    environment: {ADMIN_PASSWORD: test-password, SESSION_SECRET: %s}
    volumes:
      - ui-data:/data
      - %s/hosts.yml:/data/hosts.yml:ro
volumes:
  agent-data: {}
  ui-data: {}
`, testToken, remoteRoot, remoteRoot, remoteRoot, strings.Repeat("s", 32), remoteRoot)
		remoteCopy("compose.yml", remoteCompose)
		remoteComposeArgs := []string{"compose", "-p", "docklight-remote", "-f", remoteRoot + "/compose.yml"}
		remoteMust(append(remoteComposeArgs, "up", "-d")...)
		remoteID := func(service string) string { return remoteMust(append(remoteComposeArgs, "ps", "-q", service)...) }
		beforeRemoteAgent, beforeRemoteUI := remoteID("agent"), remoteID("ui")
		// Existing deployment enables master only after all binaries support maintenance.
		copyFile("hosts.yml", "hosts:\n  - id: local\n    name: Local\n    master: true\n    url: http://docklight-e2e-agent:8081\n    allow_http: true\n    token: "+testToken+"\n  - id: remote\n    name: Remote\n    url: http://docklight-e2e-remote:8081\n    allow_http: true\n    token: "+testToken+"\n")
		if b, err := local(append(composeOuter, "restart", "ui")...); err != nil {
			t.Fatal(b, err)
		}
		waitHealthy(t, must, "docklight-e2e-ui", "docklight-e2e-other-ui", "docklight-e2e-agent")
		waitHealthy(t, remoteMust, remoteID("agent"), remoteID("ui"))
		uiRequest := func(path, body, cookie string) (string, error) {
			method := "GET"
			if body != "" {
				method = "POST"
			}
			req, err := http.NewRequest(method, "http://docklight-e2e-ui:8080"+path, strings.NewReader(body))
			if err != nil {
				return "", err
			}
			req.Header.Set("Origin", "http://docklight-e2e-ui:8080")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Close = true
			if cookie != "" {
				req.AddCookie(&http.Cookie{Name: "docklight_session", Value: cookie})
			}
			var wire bytes.Buffer
			if err := req.Write(&wire); err != nil {
				return "", err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "docker", "--host", host, "exec", "-i", "docklight-e2e-agent", "nc", "-w", "40", "docklight-e2e-ui", "8080")
			// Keep stdin open until the HTTP response closes the connection. EOF here
			// would half-close nc and cancel the UI request's context during preflight.
			input, err := cmd.StdinPipe()
			if err != nil {
				return "", err
			}
			defer input.Close()
			output, err := cmd.StdoutPipe()
			if err != nil {
				return "", err
			}
			if err = cmd.Start(); err != nil {
				return "", err
			}
			if _, err = io.Copy(input, &wire); err != nil {
				_ = cmd.Wait()
				return "", err
			}
			res, err := http.ReadResponse(bufio.NewReader(output), req)
			if err != nil {
				_ = input.Close()
				_ = cmd.Wait()
				return "", err
			}
			defer res.Body.Close()
			content, err := io.ReadAll(res.Body)
			_ = input.Close()
			_ = cmd.Wait()
			if err != nil {
				return string(content), err
			}
			text := "Set-Cookie: " + res.Header.Get("Set-Cookie") + "\n" + string(content)
			if res.StatusCode >= 400 {
				return text, fmt.Errorf("UI HTTP %d", res.StatusCode)
			}
			return text, nil
		}
		cookiePattern := regexp.MustCompile(`Set-Cookie: docklight_session=([^;\s]+)`)
		csrfPattern := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
		var cookie string
		var lastLogin string
		deadline = time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			body, _ := uiRequest("/login", "password=test-password", "")
			lastLogin = body
			match := cookiePattern.FindStringSubmatch(body)
			if len(match) == 2 {
				cookie = match[1]
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if cookie == "" {
			logs, _ := remote("logs", "--tail", "30", "docklight-e2e-ui")
			t.Fatalf("maintenance UI did not start/login: %s\n%s", lastLogin, logs)
		}
		body, err := uiRequest("/?host=local", "", cookie)
		if err != nil {
			t.Fatal(body, err)
		}
		if strings.Contains(body, "maintenance verification unavailable") {
			logs, _ := remote("logs", "--tail", "30", "docklight-e2e-ui")
			probe, _ := remote("exec", "docklight-e2e-ui", "wget", "-S", "-O-", "--header", "Authorization: Bearer "+testToken, "http://docklight-e2e-agent:8081/v1/maintenance")
			t.Fatalf("maintenance connection preflight failed:\n%s\n%s", logs, probe)
		}
		match := csrfPattern.FindStringSubmatch(body)
		if len(match) != 2 || !strings.Contains(body, "Update all agents and UIs") {
			t.Fatalf("maintenance unavailable: %s", body)
		}
		beforeAgent := must("inspect", "--format", "{{.Id}}", "docklight-e2e-agent")
		beforeUI := must("inspect", "--format", "{{.Id}}", "docklight-e2e-ui")
		beforeOther := must("inspect", "--format", "{{.Id}}", "docklight-e2e-other-ui")
		// The test registry shares this daemon. Retain the old agent image as a
		// real external registry would, before moving its mutable local tag.
		must("tag", must("inspect", "--format", "{{.Image}}", "docklight-e2e-agent"), "docklight-e2e-agent:before-maintenance")
		// New images force replacement, not merely a successful no-op pull/up.
		for _, pair := range [][2]string{{uiImage, "ui"}, {agentImage, "agent"}} {
			copyFile("Dockerfile", "FROM "+pair[0]+"\nLABEL docklight.integration.revision=maintenance\n")
			tag := "localhost:5000/docklight-" + pair[1] + ":latest"
			if b, err := local("exec", outer, "docker", "build", "-t", tag, root); err != nil {
				t.Fatal(b, err)
			}
			must("push", tag)
		}
		body, err = uiRequest("/maintenance", url.Values{"host": {"local"}, "confirmed": {"yes"}, "selected": {"local", "remote"}, "csrf_token": {match[1]}}.Encode(), cookie)
		// Redirect or agent replacement can interrupt this request; reconcile via persisted state.
		if err != nil && !strings.Contains(body, "Location:") {
			t.Log("maintenance acceptance interrupted:", body, err)
		}
		deadline = time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			body, err = uiRequest("/?host=local", "", cookie)
			if err == nil && strings.Contains(body, "All agents and UIs verified.") {
				break
			}
			if strings.Contains(body, "Partial maintenance") || strings.Contains(body, "Master conflict") {
				t.Fatalf("maintenance failed: %s", body)
			}
			time.Sleep(time.Second)
		}
		if !strings.Contains(body, "All agents and UIs verified.") {
			t.Fatalf("maintenance did not recover after UI replacement: %s", body)
		}
		if must("inspect", "--format", "{{.Id}}", "docklight-e2e-agent") == beforeAgent || must("inspect", "--format", "{{.Id}}", "docklight-e2e-ui") == beforeUI || must("inspect", "--format", "{{.Id}}", "docklight-e2e-other-ui") == beforeOther {
			t.Fatal("maintenance did not replace agent and all UIs")
		}
		if remoteID("agent") == beforeRemoteAgent || remoteID("ui") == beforeRemoteUI {
			t.Fatal("remote maintenance did not replace both services")
		}
		body, err = request("/v1/maintenance", "")
		var info MaintenanceInfo
		if err != nil || json.Unmarshal([]byte(body), &info) != nil || info.State.Run != "" || info.State.Owner.Coordinator == "" {
			t.Fatalf("lost registration or leaked lock: %s %v", body, err)
		}
	})
}

func envTest(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
