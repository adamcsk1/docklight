package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"docklight/internal/management"
	"github.com/google/uuid"
)

func TestActionAcceptanceSurvivesBrowserInterruption(t *testing.T) {
	for _, interruption := range []string{"cancel", "deadline"} {
		t.Run(interruption, func(t *testing.T) {
			a := newTestApp(t)
			cookie := session(t, a)
			entered, release := make(chan *http.Request, 1), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			id := uuid.NewString()
			a.cfg.Hosts = []Host{{ID: "local", URL: "http://agent", Token: strings.Repeat("s", 32), client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				entered <- r
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{"ID":"` + id + `","Status":"running"}`)), Header: make(http.Header)}, nil
			})}}}
			ctx, cancel := context.WithCancel(context.Background())
			if interruption == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			}
			defer cancel()
			r := formRequest(a, cookie, "/action", url.Values{"host": {"local"}, "kind": {"container"}, "id": {"target"}, "operation": {"stop"}, "request_id": {id}}).WithContext(ctx)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); a.routes().ServeHTTP(w, r) }()
			var acceptance *http.Request
			select {
			case acceptance = <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("agent acceptance did not start")
			}
			if interruption == "cancel" {
				cancel()
			}
			<-ctx.Done()
			if interruption == "deadline" && ctx.Err() != context.DeadlineExceeded {
				t.Fatal("tested cancellation instead of deadline expiry")
			}
			if acceptance.Context().Err() != nil {
				t.Error("browser interruption cancelled acceptance")
			}
			deadline, bounded := acceptance.Context().Deadline()
			if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > actionTimeout {
				t.Error("acceptance is not independently bounded")
			}
			if acceptance.Header.Get("X-Docklight-Request") != id {
				t.Error("lost request identity")
			}
			once.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("acceptance did not finish")
			}
			if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "job="+id) {
				t.Fatalf("acceptance lost after %s: %d %s", interruption, w.Code, w.Body.String())
			}
		})
	}
}

func TestUIReconnectRestoresAgentOperationWithoutJobURL(t *testing.T) {
	a := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := uuid.NewString()
	var mu sync.Mutex
	var job *management.Job
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/jobs":
			if r.Header.Get("X-Docklight-Request") != id {
				t.Error("UI lost submission identity")
			}
			job = &management.Job{ID: id, Status: "running", Action: management.Action{Kind: "container", Operation: "upgrade"}, Output: "pulling image"}
			cancel() // Browser closes; the agent has accepted, but its response is lost.
			http.Error(w, "response lost", 502)
		case "/v1/jobs/current":
			_ = json.NewEncoder(w).Encode(job)
		case "/v1/inventory":
			state := "running"
			if job != nil && job.Status == "success" {
				state = "exited"
			}
			_ = json.NewEncoder(w).Encode(management.Inventory{Containers: []management.Container{{ID: "target", Name: "web", State: state}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer agent.Close()
	a.cfg.Hosts = []Host{{ID: "local", Name: "Local", URL: agent.URL, Token: strings.Repeat("s", 32), AllowHTTP: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	cookie := session(t, a)
	r := formRequest(a, cookie, "/action", url.Values{"host": {"local"}, "kind": {"container"}, "id": {"target"}, "operation": {"upgrade"}, "scope": {"stack"}, "request_id": {id}}).WithContext(ctx)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "outcome may be unknown") {
		t.Fatalf("lost acceptance misreported: %d %s", w.Code, w.Body.String())
	}
	// Nothing from the original UI process or browser URL is needed for rediscovery.
	restarted := &App{cfg: a.cfg, db: a.db}
	if err := restarted.initDB(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "success"} {
		mu.Lock()
		job.Status = status
		mu.Unlock()
		r := httptest.NewRequest("GET", a.withBase("/")+"?host=local", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		restarted.routes().ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `data-status="`+status+`"`) || !strings.Contains(body, "job="+id) || !strings.Contains(body, "pulling image") {
			t.Fatalf("operation not restored: %d %s", w.Code, body)
		}
		if status == "success" && !strings.Contains(body, "badge-exited") {
			t.Fatal("completed job restored beside stale inventory")
		}
	}
}

func TestReadPathsDoNotVerifyFleetOrHideInventoryOnConflict(t *testing.T) {
	a, _, _ := maintenanceApp(t)
	cookie := session(t, a)
	for _, conflict := range []bool{false, true} {
		if conflict {
			if err := a.maintenanceValue("conflict", "known conflict"); err != nil {
				t.Fatal(err)
			}
		}
		a.maintenanceMu.Lock()
		for _, path := range []string{"/", "/inventory"} {
			r := httptest.NewRequest("GET", a.withBase(path)+"?host=main", nil)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "data-inventory ") {
				t.Errorf("read blocked by verification/conflict: %d %s", w.Code, w.Body.String())
			}
		}
		a.maintenanceMu.Unlock()
		if !a.verifiedAt.IsZero() {
			t.Fatal("read-only request triggered fleet verification")
		}
	}
}

func TestMaintenanceViewFallsBackWhenSwitchingToNonMaster(t *testing.T) {
	a, _, _ := maintenanceApp(t)
	r := httptest.NewRequest("GET", a.withBase("/")+"?host=remote&view=maintenance", nil)
	r.AddCookie(session(t, a))
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != a.withBase("/")+"?host=remote" {
		t.Fatalf("host switch left a blank maintenance screen: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestBrowserOperationRecovery(t *testing.T) {
	if os.Getenv("DOCKLIGHT_UI_BROWSER_TEST") != "1" {
		t.Skip("requires Playwright")
	}
	a := newTestApp(t)
	var mu sync.Mutex
	var job *management.Job
	submissions := 0
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/inventory":
			name := "web-original"
			if job != nil && job.Status == "success" {
				name = "web-updated"
			}
			resources := []management.Resource{{ID: "recovery-volume", Name: "recovery-volume", Kind: "volume"}}
			if job != nil && job.Action.Kind == "volume" && job.Status == "success" {
				resources = nil
			}
			_ = json.NewEncoder(w).Encode(management.Inventory{Containers: []management.Container{{ID: strings.Repeat("a", 64), Name: name, State: "running", Project: "web", ComposeAvailable: true}}, Resources: resources})
		case r.URL.Path == "/v1/jobs" && r.Method == "POST":
			var action management.Action
			if err := json.NewDecoder(r.Body).Decode(&action); err != nil {
				http.Error(w, "bad action", 400)
				return
			}
			submissions++
			job = &management.Job{ID: r.Header.Get("X-Docklight-Request"), Status: "running", Output: "Compose pull running", Action: action}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(job)
		case strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(job)
		default:
			http.NotFound(w, r)
		}
	}))
	defer agent.Close()
	a.cfg.Hosts = []Host{{ID: "local", Name: "Local", URL: agent.URL, Token: strings.Repeat("s", 32), AllowHTTP: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	handler := a.routes()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case a.withBase("/test-ui-restart"):
			restarted := &App{cfg: a.cfg, db: a.db}
			if err := restarted.initDB(); err != nil {
				t.Error(err)
			}
			if err := restarted.loadTemplates(); err != nil {
				t.Error(err)
			}
			mu.Lock()
			handler = restarted.routes()
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case a.withBase("/test-finish"):
			mu.Lock()
			job.Status, job.Output = "success", "Compose upgrade completed"
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			mu.Lock()
			current := handler
			mu.Unlock()
			current.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	cmd := exec.Command("node", "../../scripts/operation-recovery-smoke.cjs", server.URL+a.withBase(""))
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if submissions != 2 {
		t.Fatalf("expected one upgrade and one deletion: %d submissions", submissions)
	}
}
