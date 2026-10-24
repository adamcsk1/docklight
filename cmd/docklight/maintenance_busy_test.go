package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlowMaintenanceVerificationWaitsThenSubmitsAction(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	cookie := session(t, a)
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	var submissions atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/maintenance" {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		if r.Method == "POST" && r.URL.Path == "/v1/jobs" {
			submissions.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"ID":"slow-verification-job","Status":"running"}`))
			return
		}
		agents[0].serve(w, r)
	}))
	defer slow.Close()
	a.cfg.Hosts[0].URL = slow.URL
	tickDone := make(chan struct{})
	go func() { defer close(tickDone); a.maintenanceTick(context.Background()) }()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-tickDone:
		case <-time.After(5 * time.Second):
			t.Error("verification did not finish during cleanup")
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("verification never reached slow agent")
	}
	w := httptest.NewRecorder()
	actionEntered := make(chan struct{})
	var actionEnteredOnce sync.Once
	routes := a.routes()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == a.withBase("/action") {
			actionEnteredOnce.Do(func() { close(actionEntered) })
		}
		routes.ServeHTTP(w, r)
	})
	actionDone := make(chan struct{})
	go func() {
		defer close(actionDone)
		handler.ServeHTTP(w, formRequest(a, cookie, "/action", url.Values{"host": {"main"}, "kind": {"container"}, "id": {"main-agent-id"}, "operation": {"stop"}}))
	}()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-actionDone:
		case <-time.After(5 * time.Second):
			t.Error("action did not finish during cleanup")
		}
	}()
	select {
	case <-actionEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("action never reached server")
	}
	select {
	case <-actionDone:
		t.Fatal("action completed while verification held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	if submissions.Load() != 0 {
		t.Fatal("action reached agent before verification finished")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-actionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("action did not resume after verification")
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != a.withBase("/")+"?host=main&job=slow-verification-job" {
		t.Fatalf("delayed action failed: %d %s location=%q", w.Code, w.Body.String(), w.Header().Get("Location"))
	}
	if submissions.Load() != 1 {
		t.Fatalf("delayed action submissions = %d, want 1", submissions.Load())
	}
	for _, agent := range agents {
		agent.mu.Lock()
		posts := agent.posts
		agent.mu.Unlock()
		if posts != 0 {
			t.Fatal("manual action became an unowned maintenance job")
		}
	}
}

func TestActionWaitInterruptionDoesNotSubmit(t *testing.T) {
	for _, interruption := range []string{"timeout", "cancel"} {
		t.Run(interruption, func(t *testing.T) {
			a, actions := presentationApp(t)
			a.cfg.Hosts[1].Master = true
			cookie := session(t, a)
			a.maintenanceMu.Lock()
			r := formRequest(a, cookie, "/action", url.Values{"host": {"pi"}, "kind": {"container"}, "id": {"stopped-id"}, "operation": {"start"}})
			var cancel context.CancelFunc
			if interruption == "timeout" {
				var ctx context.Context
				ctx, cancel = context.WithTimeout(r.Context(), 100*time.Millisecond)
				r = r.WithContext(ctx)
			} else {
				var ctx context.Context
				ctx, cancel = context.WithCancel(r.Context())
				r = r.WithContext(ctx)
			}
			defer cancel()
			entered := make(chan struct{})
			var enteredOnce sync.Once
			routes := a.routes()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == a.withBase("/action") {
					enteredOnce.Do(func() { close(entered) })
				}
				routes.ServeHTTP(w, r)
			})
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				handler.ServeHTTP(w, r)
			}()
			defer func() {
				a.maintenanceMu.Unlock()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("interrupted action did not finish during cleanup")
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("action never reached server")
			}
			if interruption == "cancel" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("interrupted action stayed blocked while verification held the lock")
			}
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("interrupted action status = %d, want %d", w.Code, http.StatusServiceUnavailable)
			}
			if len(actions) != 0 {
				t.Fatal("interrupted action reached agent")
			}
		})
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
			r := httptest.NewRequest("GET", a.withBase("/")+"?host=pi", nil)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "data-inventory ") || !strings.Contains(w.Body.String(), `value="pi" selected`) {
				t.Fatalf("busy dashboard unavailable: %d %s", w.Code, w.Body.String())
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
			actionEntered := make(chan struct{}, 2)
			// Test endpoints hold the same lock as a slow background verification.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case a.withBase("/test-verification-busy"):
					a.maintenanceMu.Lock()
					w.WriteHeader(204)
				case a.withBase("/test-verification-idle"):
					a.maintenanceMu.Unlock()
					w.WriteHeader(204)
				case a.withBase("/test-action-entered"):
					select {
					case <-actionEntered:
						w.WriteHeader(204)
					case <-r.Context().Done():
					}
				default:
					if r.Method == http.MethodPost && r.URL.Path == a.withBase("/action") {
						actionEntered <- struct{}{}
					}
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
			if len(actions) != 2 {
				t.Fatalf("delayed browser actions = %d, want exactly one per JavaScript mode", len(actions))
			}
		})
	}
}
