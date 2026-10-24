package management

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestAgentHealthEndpointBypassesOnlyExactGetRoute(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong", testToken} {
		w := agentRequest(s, "GET", "/healthz", "", token)
		if w.Code != http.StatusOK || w.Body.String() != "ok" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("health response: %d %q", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("missing health response headers: %v", w.Header())
		}
		if strings.Contains(w.Body.String(), testToken) {
			t.Fatal("health response leaked agent token")
		}
	}
	for _, request := range []struct{ method, path string }{
		{"GET", "/healthz/"},
		{"GET", "/healthz/inventory"},
		{"GET", "/v1/inventory"},
		{"GET", "/v1/maintenance"},
		{"GET", "/v1/jobs/some-id"},
		{"POST", "/v1/jobs"},
		{"POST", "/healthz"},
	} {
		if w := agentRequest(s, request.method, request.path, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s: HTTP %d", request.method, request.path, w.Code)
		}
	}
	if w := agentRequest(s, "POST", "/healthz", "", testToken); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("authenticated POST /healthz: HTTP %d", w.Code)
	}
	if w := agentRequest(s, "GET", "/healthz/", "", testToken); w.Code != http.StatusNotFound {
		t.Fatalf("authenticated non-exact health path: HTTP %d", w.Code)
	}
	if len(f.calls) != 0 {
		t.Fatalf("health or rejected requests called Docker: %v", f.calls)
	}
}

func TestDockerfileImageHealthchecks(t *testing.T) {
	content, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(content), "\\\r\n", " ")
	text = strings.ReplaceAll(text, "\\\n", " ")
	checks := map[string]string{}
	stage := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "FROM ") {
			stage = ""
			if i := strings.LastIndex(line, " AS "); i >= 0 {
				stage = line[i+4:]
			}
		}
		if strings.HasPrefix(line, "HEALTHCHECK ") {
			if checks[stage] != "" {
				t.Fatalf("duplicate healthcheck in %s stage", stage)
			}
			checks[stage] = line
		}
	}
	if len(checks) != 2 {
		t.Fatalf("got healthchecks for %v, want agent and ui only", checks)
	}
	for _, stage := range []string{"agent", "ui"} {
		check := checks[stage]
		for _, option := range []string{"--interval=30s", "--timeout=5s", "--start-period=20s", "--retries=3"} {
			if !strings.Contains(check, option) {
				t.Errorf("%s healthcheck missing %s: %s", stage, option, check)
			}
		}
		for _, secret := range []string{"AGENT_TOKEN", "ADMIN_PASSWORD", "SESSION_SECRET", "Authorization"} {
			if strings.Contains(check, secret) {
				t.Errorf("%s healthcheck may expose %s", stage, secret)
			}
		}
	}
	for _, probe := range []string{"wget", "docklight-agent -healthcheck", "${ADDR:-:8081}", "host=localhost", "/healthz", "docker info"} {
		if !strings.Contains(checks["agent"], probe) {
			t.Errorf("agent healthcheck missing %s", probe)
		}
	}
	for _, probe := range []string{"wget", "${ADDR:-:8080}", "addr=${addr:-:8080}", "${BASE_URL:-/}", "${base#/}", "${base%/}/healthz"} {
		if !strings.Contains(checks["ui"], probe) {
			t.Errorf("UI healthcheck missing %s", probe)
		}
	}
}
