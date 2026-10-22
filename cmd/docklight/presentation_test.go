package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"docklight/internal/management"
)

func presentationApp(t *testing.T) (*App, chan management.Action) {
	t.Helper()
	a := newTestApp(t)
	actions := make(chan management.Action, 32)
	inv := management.Inventory{
		Containers: []management.Container{
			{ID: "running-id", Name: "web-running", Image: "registry.example/web:latest", State: "running", Project: "web", Service: "web", ComposeAvailable: true, BuildError: "Compose config unavailable; check mounted files, .env and required variables on the agent"},
			{ID: "stopped-id", Name: "web-stopped", Image: "local-worker:latest", State: "exited", Project: "web", Service: "worker", ComposeAvailable: true, Buildable: true},
			{ID: "agent-id", Name: "docklight-agent", Image: "ghcr.io/adamcsk1/docklight-agent:latest", State: "running", Project: "docklight", Service: "agent", Protected: true, ComposeAvailable: true},
			{ID: "standalone-id", Name: "standalone", Image: strings.Repeat("very-long-image-name-", 12), State: "exited"},
		},
		Resources: []management.Resource{
			{Kind: "image", ID: "image-id", Name: "web:latest", UsedBy: []string{"web-running", "web-stopped"}},
			{Kind: "image", ID: "unused-image-id", Name: "old-web:previous"},
			{Kind: "volume", ID: "used-volume", Name: "used-volume", UsedBy: []string{"web-stopped"}},
			{Kind: "volume", ID: "unused-volume", Name: "unused-volume"},
			{Kind: "network", ID: "bridge-id", Name: "bridge", Protected: true},
			{Kind: "network", ID: "unused-network-id", Name: "unused-network"},
		},
	}
	var successPolls, failedPolls, expiredPolls atomic.Int32
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/inventory":
			_ = json.NewEncoder(w).Encode(inv)
		case r.URL.Path == "/v1/jobs" && r.Method == "POST":
			var action management.Action
			if err := json.NewDecoder(r.Body).Decode(&action); err != nil {
				http.Error(w, "bad action", 400)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("p", 32) {
				t.Error("browser mutation reached the wrong host")
			}
			actions <- action
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(management.Job{ID: "presentation-job"})
		case r.URL.Path == "/v1/jobs":
			t.Error("dashboard requested removed operation history")
			http.Error(w, "history removed", http.StatusMethodNotAllowed)
		case strings.HasPrefix(r.URL.Path, "/v1/jobs/live-"):
			counter := &successPolls
			terminal := "success"
			if strings.HasSuffix(r.URL.Path, "failed") {
				counter = &failedPolls
				terminal = "failed"
			}
			if strings.HasSuffix(r.URL.Path, "expired") {
				counter = &expiredPolls
			}
			n := counter.Add(1)
			if n == 4 {
				http.Error(w, "agent temporarily restarting", 502)
				return
			}
			status := "running"
			if n == 5 {
				status = "updating"
			}
			if n >= 6 {
				status = terminal
			}
			tail := status + " output"
			if n == 1 {
				tail = "initial output"
			}
			_ = json.NewEncoder(w).Encode(management.Job{ID: strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), Status: status, Action: management.Action{Kind: "container", Operation: "build"}, Output: strings.Repeat("build log line\n", 100) + "<script>not executable</script>\n" + tail})
		default:
			_ = json.NewEncoder(w).Encode(management.Job{ID: "presentation-job", Status: "success", Output: strings.Repeat("long-output-without-spaces", 30) + "\n<script>not executable</script>"})
		}
	}))
	t.Cleanup(agent.Close)
	a.cfg.Hosts = []Host{{ID: "local", Name: "Local server", URL: agent.URL, Token: strings.Repeat("l", 32), AllowHTTP: true}, {ID: "pi", Name: "Raspberry Pi", URL: agent.URL, Token: strings.Repeat("p", 32), AllowHTTP: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	return a, actions
}

func TestRestoredPresentationAndEmbeddedAssets(t *testing.T) {
	a, _ := presentationApp(t)
	for _, base := range []string{"/", "/docklight"} {
		a.cfg.BaseURL = base
		cookie := &http.Cookie{Name: "docklight_session", Value: a.signSessionID("presentation")}
		if _, err := a.db.Exec(`INSERT OR REPLACE INTO sessions(id_hash,expires_at) VALUES(?,'2099-01-01T00:00:00Z')`, a.sessionIDHash("presentation")); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "https://docklight.local"+a.withBase("/")+"?host=pi", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, part := range []string{`class="topbar"`, `class="brand"`, `class="page"`, `class="resource-table"`, `class="button button-primary"`, `class="badge badge-running"`, `class="badge badge-exited"`, `value="pi" selected`, `href="` + a.withBase("/static/app.css") + `"`, `name="csrf_token" value="` + a.csrfToken(cookie.Value) + `"`, `name="host" value="pi"`} {
			if !strings.Contains(body, part) {
				t.Fatalf("missing restored presentation: %s", part)
			}
		}
		for _, id := range []string{"containers", "images", "volumes", "networks"} {
			if strings.Count(body, `id="`+id+`"`) != 1 {
				t.Fatalf("missing/duplicate section %s", id)
			}
		}
		for _, forbidden := range []string{`style=`, `<style`, `<script>`, `htmx`, `name="username"`, `/exec`, `/stacks/new`, `activity`, `Activity`, `legacy history`, `history-panel`} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("obsolete/inline content: %s", forbidden)
			}
		}
		for _, label := range []string{`aria-label="Compose stack docklight"`, `aria-label="Compose stack web"`, `aria-label="Standalone containers"`, `1 / 2 running`} {
			if !strings.Contains(body, label) {
				t.Fatalf("missing stack grouping: %s", label)
			}
		}
		if strings.Contains(body, `class="stack-group" open`) || strings.Count(body, `class="stack-group"`) != 3 {
			t.Fatal("stack groups must all start collapsed")
		}
		if strings.Count(body, "<script") != 1 || !strings.Contains(body, `<script src="`+a.withBase("/static/app.js")+`" defer></script>`) {
			t.Fatal("dashboard must load only the external accordion script")
		}
		if !strings.Contains(body, `data-host="pi" data-project="web"`) {
			t.Fatal("missing host/stack storage scope")
		}
		for path, contentType := range map[string]string{"/static/app.css": "text/css", "/static/app.js": "text/javascript", "/static/logo.ico": "image/"} {
			asset := httptest.NewRecorder()
			a.routes().ServeHTTP(asset, httptest.NewRequest("GET", "https://docklight.local"+a.withBase(path), nil))
			if asset.Code != 200 || !strings.HasPrefix(asset.Header().Get("Content-Type"), contentType) {
				t.Fatalf("asset %s: %d %s", path, asset.Code, asset.Header().Get("Content-Type"))
			}
			if path == "/static/app.css" {
				for _, token := range []string{"--color-bg: #08090b", "--color-blue: #2496ed", "--font-family: monospace", "@media", ":focus-visible"} {
					if !strings.Contains(asset.Body.String(), token) {
						t.Fatalf("missing original design token: %s", token)
					}
				}
			}
		}
	}
}

// Opt-in browser checks use an isolated fake agent and never contact Docker.
func TestBrowserPresentation(t *testing.T) {
	if os.Getenv("DOCKLIGHT_UI_BROWSER_TEST") != "1" {
		t.Skip("set DOCKLIGHT_UI_BROWSER_TEST=1 with Playwright installed")
	}
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a, actions := presentationApp(t)
			a.cfg.BaseURL = base
			server := httptest.NewServer(a.routes())
			defer server.Close()
			cmd := exec.Command("node", "../../scripts/ui-smoke.cjs", server.URL+a.withBase(""))
			output, err := cmd.CombinedOutput()
			t.Log(string(output))
			if err != nil {
				t.Fatal(err)
			}
			if len(actions) != 12 {
				t.Fatalf("unexpected browser mutation count: %d", len(actions))
			}
			for i := 0; i < 12; i++ {
				action := <-actions
				if i%3 == 0 {
					if action.Kind != "container" || action.Operation != "start" || action.ID != "stopped-id" || action.Scope != "service" {
						t.Fatalf("wrong start payload: %#v", action)
					}
				} else if i%3 == 1 {
					if action.Kind != "container" || action.Operation != "build" || action.ID != "stopped-id" || action.Scope != "service" {
						t.Fatalf("wrong build payload: %#v", action)
					}
				} else if action.Kind != "volume" || action.Operation != "delete" || action.ID != "unused-volume" || !action.Confirmed {
					t.Fatalf("wrong cleanup payload: %#v", action)
				}
			}
		})
	}
}
