package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSlowMaintenanceVerificationDoesNotReplaceDashboard(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	cookie := session(t, a)
	entered, release := make(chan struct{}), make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/maintenance" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		agents[0].serve(w, r)
	}))
	defer slow.Close()
	a.cfg.Hosts[0].URL = slow.URL
	done := make(chan struct{})
	go func() { defer close(done); a.maintenanceTick(context.Background()) }()
	defer func() { close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("verification never reached slow agent")
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, cookie, "/action", url.Values{"host": {"main"}, "kind": {"container"}, "id": {"main-agent-id"}, "operation": {"stop"}}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("slow verification returned raw error: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", w.Header().Get("Location"), nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "No operation was submitted.") || !strings.Contains(w.Body.String(), "data-inventory ") {
		t.Fatal("dashboard did not survive slow verification")
	}
	for _, agent := range agents {
		agent.mu.Lock()
		posts := agent.posts
		agent.mu.Unlock()
		if posts != 0 {
			t.Fatal("busy request changed Docker")
		}
	}
}

func TestMaintenanceVerificationBusyKeepsDashboard(t *testing.T) {
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a, actions := presentationApp(t)
			a.cfg.BaseURL = base
			a.cfg.Hosts[1].Master = true
			cookie := session(t, a)
			a.maintenanceMu.Lock()
			defer a.maintenanceMu.Unlock()
			handler := a.routes()
			actionRequest := func(values url.Values) *http.Request {
				r := formRequest(a, cookie, "/action", values)
				r.URL.Path = a.withBase("/action")
				return r
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, actionRequest(url.Values{"host": {"pi"}, "kind": {"container"}, "id": {"stopped-id"}, "operation": {"start"}}))
			if w.Code != http.StatusSeeOther {
				t.Fatalf("busy action replaced dashboard: %d %s", w.Code, w.Body.String())
			}
			location := w.Header().Get("Location")
			if location != a.withBase("/")+"?host=pi&notice=maintenance-busy" {
				t.Fatalf("lost selected host or notice: %s", location)
			}
			r := httptest.NewRequest("GET", location, nil)
			r.AddCookie(cookie)
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "No operation was submitted.") || !strings.Contains(w.Body.String(), "data-inventory ") || !strings.Contains(w.Body.String(), `value="pi" selected`) {
				t.Fatalf("busy dashboard unavailable: %d %s", w.Code, w.Body.String())
			}
			if len(actions) != 0 {
				t.Fatal("busy action reached agent")
			}
			for _, operation := range []string{"stop", "delete", "update"} {
				w = httptest.NewRecorder()
				handler.ServeHTTP(w, actionRequest(url.Values{"host": {"pi"}, "kind": {"container"}, "id": {"stopped-id"}, "operation": {operation}, "confirmed": {"yes"}}))
				if w.Code != http.StatusSeeOther || len(actions) != 0 {
					t.Fatalf("busy %s was submitted or lost dashboard", operation)
				}
			}
			r = httptest.NewRequest("GET", a.withBase("/inventory")+"?host=pi", nil)
			r.AddCookie(cookie)
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "3" {
				t.Fatalf("busy inventory not retryable: %d %v", w.Code, w.Header())
			}
			for _, values := range []url.Values{
				{"kind": {"container"}, "id": {"stopped-id"}, "operation": {"start"}},
				{"host": {"unknown"}, "kind": {"container"}, "id": {"stopped-id"}, "operation": {"start"}},
				{"host": {"pi"}, "kind": {"container"}, "id": {"stopped-id"}, "operation": {"invalid"}},
			} {
				w = httptest.NewRecorder()
				handler.ServeHTTP(w, actionRequest(values))
				if w.Code != http.StatusBadRequest {
					t.Fatalf("invalid action disguised as busy: %d", w.Code)
				}
			}
			badCSRF := actionRequest(url.Values{"host": {"pi"}})
			if err := badCSRF.ParseForm(); err != nil {
				t.Fatal(err)
			}
			badCSRF.Form.Set("csrf_token", "invalid")
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, badCSRF)
			if w.Code != http.StatusForbidden {
				t.Fatal("busy path bypassed CSRF")
			}
			// Installations without a master do not participate in verification locking.
			a.cfg.Hosts[1].Master = false
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, actionRequest(url.Values{"host": {"pi"}, "kind": {"container"}, "id": {"stopped-id"}, "operation": {"start"}}))
			if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "job=presentation-job") || len(actions) != 1 {
				t.Fatal("non-master deployment blocked by verification lock")
			}
		})
	}
}

func TestBrowserMaintenanceBusy(t *testing.T) {
	if os.Getenv("DOCKLIGHT_UI_BROWSER_TEST") != "1" {
		t.Skip("requires Playwright")
	}
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a, actions := presentationApp(t)
			a.cfg.BaseURL = base
			a.cfg.Hosts[1].Master = true
			handler := a.routes()
			// Test endpoints hold the same lock as a slow background verification.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case a.withBase("/test-verification-busy"):
					a.maintenanceMu.Lock()
					w.WriteHeader(204)
				case a.withBase("/test-verification-idle"):
					a.maintenanceMu.Unlock()
					w.WriteHeader(204)
				default:
					handler.ServeHTTP(w, r)
				}
			}))
			defer server.Close()
			cmd := exec.Command("node", "../../scripts/maintenance-busy-smoke.cjs", server.URL+a.withBase(""))
			out, err := cmd.CombinedOutput()
			t.Log(string(out))
			if err != nil {
				t.Fatal(err)
			}
			if len(actions) != 0 {
				t.Fatal("rejected browser action was submitted or replayed")
			}
		})
	}
}
