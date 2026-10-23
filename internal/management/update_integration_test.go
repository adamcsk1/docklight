package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
  agent-data: {}
`, strings.Repeat("s", 32), root, testToken, root, root, root)
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
}

func envTest(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
