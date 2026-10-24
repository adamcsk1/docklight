package main

import (
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"docklight/internal/management"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	a := &App{db: db, cfg: Config{BaseURL: "/docklight", AdminPassword: "pw", SessionSecret: strings.Repeat("s", 32)}}
	if err = a.initDB(); err != nil {
		t.Fatal(err)
	}
	if err = a.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	return a
}
func session(t *testing.T, a *App) *http.Cookie {
	t.Helper()
	id := "test-session"
	if _, err := a.db.Exec(`INSERT INTO sessions(id_hash,expires_at) VALUES(?,?)`, a.sessionIDHash(id), time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "docklight_session", Value: a.signSessionID(id)}
}
func formRequest(a *App, cookie *http.Cookie, path string, values url.Values) *http.Request {
	if values == nil {
		values = url.Values{}
	}
	if cookie != nil {
		values.Set("csrf_token", a.csrfToken(cookie.Value))
	}
	r := httptest.NewRequest("POST", "https://docklight.local/docklight"+path, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://docklight.local")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func TestLoginSessionsCSRFOriginAndLogout(t *testing.T) {
	a := newTestApp(t)
	handler := a.routes()
	r := formRequest(a, nil, "/login", url.Values{"password": {"pw"}})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 302 {
		t.Fatalf("login %d: %s", w.Code, w.Body.String())
	}
	c := w.Result().Cookies()[0]
	if !c.HttpOnly || !c.Secure || c.Path != "/docklight" || c.SameSite != http.SameSiteLaxMode || !a.validSession(c.Value) || a.validSession(c.Value+"tamper") {
		t.Fatalf("unsafe session %#v", c)
	}
	for _, tc := range []struct{ origin, csrf string }{{"https://evil.local", "valid"}, {"http://docklight.local", "valid"}, {"", "valid"}, {"https://docklight.local", "bad"}} {
		r = formRequest(a, c, "/logout", nil)
		r.Header.Set("Origin", tc.origin)
		if tc.csrf == "bad" {
			r = formRequest(a, nil, "/logout", url.Values{"csrf_token": {"bad"}})
			r.AddCookie(c)
		}
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("bad origin/csrf accepted %d", w.Code)
		}
	}
	r = formRequest(a, c, "/logout", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 302 || a.validSession(c.Value) {
		t.Fatal("logout failed")
	}
}

func TestPasswordOnlyFormAndRejectedPasswords(t *testing.T) {
	a := newTestApp(t)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest("GET", "https://docklight.local/docklight/login", nil))
	if strings.Contains(w.Body.String(), `name="username"`) || !strings.Contains(w.Body.String(), `name="password"`) {
		t.Fatal("login form is not password-only")
	}
	for _, password := range []string{"", "incorrect"} {
		w = httptest.NewRecorder()
		a.routes().ServeHTTP(w, formRequest(a, nil, "/login", url.Values{"password": {password}}))
		if w.Code == 302 || len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "Invalid password") {
			t.Fatal("invalid password created a session")
		}
	}
}

func TestLogoutFailureRetainsSessionForRetry(t *testing.T) {
	a := newTestApp(t)
	c := session(t, a)
	if _, err := a.db.Exec(`CREATE TRIGGER reject_logout BEFORE DELETE ON sessions BEGIN SELECT RAISE(FAIL, 'storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, c, "/logout", nil))
	if w.Code != http.StatusServiceUnavailable || len(w.Result().Cookies()) != 0 || !a.validSession(c.Value) {
		t.Fatalf("logout concealed revocation failure: %d %s", w.Code, w.Body.String())
	}
	if _, err := a.db.Exec(`DROP TRIGGER reject_logout`); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, c, "/logout", nil))
	if w.Code != http.StatusFound || a.validSession(c.Value) {
		t.Fatal("logout retry failed")
	}
}

func TestAuthenticatedLoginPageRedirects(t *testing.T) {
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a := newTestApp(t)
			a.cfg.BaseURL = base
			cookie := session(t, a)
			r := httptest.NewRequest("GET", "https://docklight.local"+a.withBase("/login"), nil)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			if w.Code != http.StatusFound || w.Header().Get("Location") != a.withBase("/") {
				t.Fatalf("authenticated user shown login page: HTTP %d, location %q", w.Code, w.Header().Get("Location"))
			}
			if !a.validSession(cookie.Value) {
				t.Fatal("opening login page invalidated session")
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("opening login page changed the existing session cookie")
			}
		})
	}
}

func TestLoginPageDoesNotRedirectInvalidSessions(t *testing.T) {
	for _, state := range []string{"missing", "tampered", "expired", "revoked", "storage-error"} {
		t.Run(state, func(t *testing.T) {
			a := newTestApp(t)
			cookie := session(t, a)
			switch state {
			case "tampered":
				cookie.Value += "tampered"
			case "expired":
				if _, err := a.db.Exec(`UPDATE sessions SET expires_at=?`, time.Now().Add(-time.Hour).Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if _, err := a.db.Exec(`DELETE FROM sessions`); err != nil {
					t.Fatal(err)
				}
			case "storage-error":
				if _, err := a.db.Exec(`DROP TABLE sessions`); err != nil {
					t.Fatal(err)
				}
			}
			r := httptest.NewRequest("GET", "https://docklight.local/docklight/login", nil)
			if state != "missing" {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="password"`) || w.Header().Get("Location") != "" {
				t.Fatalf("invalid session bypassed login: %d %s", w.Code, w.Body.String())
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("invalid session was refreshed")
			}
		})
	}
}

func TestHealthEndpointIsMinimalAndUnauthenticated(t *testing.T) {
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a := newTestApp(t)
			a.cfg.BaseURL = base
			a.cfg.Hosts = []Host{{ID: "private-host", Name: "private-name", Token: "private-agent-token"}}
			handler := a.routes()
			db := a.db
			a.db = nil
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "https://docklight.local"+a.withBase("/healthz"), nil))
			a.db = db
			if w.Code != http.StatusOK || w.Body.String() != "ok" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("health response: %d %q", w.Code, w.Body.String())
			}
			for name, want := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Content-Security-Policy": "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"} {
				if got := w.Header().Get(name); got != want {
					t.Fatalf("%s: %q", name, got)
				}
			}
			if len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "private") {
				t.Fatal("health response created a session or leaked configuration")
			}
			for _, method := range []string{"POST", "PUT", "DELETE"} {
				w = httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest(method, "https://docklight.local"+a.withBase("/healthz"), nil))
				if w.Code != http.StatusMethodNotAllowed {
					t.Fatalf("%s health: HTTP %d", method, w.Code)
				}
			}
			wrongBase := "/healthz"
			if base == "/" {
				wrongBase = "/docklight/healthz"
			}
			for _, path := range []string{wrongBase, a.withBase("/healthz/")} {
				w = httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest("GET", "https://docklight.local"+path, nil))
				if w.Code != http.StatusNotFound {
					t.Fatalf("non-exact health path %s: HTTP %d", path, w.Code)
				}
			}
			for _, path := range []string{"/", "/inventory"} {
				w = httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest("GET", "https://docklight.local"+a.withBase(path), nil))
				if w.Code != http.StatusFound || w.Header().Get("Location") != a.withBase("/login") {
					t.Fatalf("protected UI route %s lost authentication: HTTP %d", path, w.Code)
				}
			}
		})
	}
}

func TestCSPAllowsOnlySameOriginAssets(t *testing.T) {
	a := newTestApp(t)
	for _, base := range []string{"/", "/docklight"} {
		a.cfg.BaseURL = base
		for _, path := range []string{"/login", "/static/app.css", "/static/app.js", "/static/logo.ico"} {
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, httptest.NewRequest("GET", "https://docklight.local"+a.withBase(path), nil))
			if w.Code != http.StatusOK {
				t.Fatalf("%s: HTTP %d", path, w.Code)
			}
			policy := w.Header().Get("Content-Security-Policy")
			want := "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
			if policy != want {
				t.Fatalf("unexpected CSP: %q", policy)
			}
			if strings.Contains(policy, "'unsafe-inline'") || strings.Contains(policy, "'unsafe-eval'") || strings.Contains(policy, "'unsafe-hashes'") || strings.Contains(policy, "'sha256-") {
				t.Fatal("CSP broadly permits inline content")
			}
		}
	}
}

func TestLoginThrottleAndExpiry(t *testing.T) {
	a := newTestApp(t)
	for i := 0; i < 8; i++ {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, formRequest(a, nil, "/login", url.Values{"password": {"wrong"}}))
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, nil, "/login", url.Values{"password": {"pw"}}))
	if !strings.Contains(w.Body.String(), "Too many failed login attempts") {
		t.Fatal("no login throttle")
	}
	c := session(t, a)
	if _, err := a.db.Exec(`UPDATE sessions SET expires_at=?`, time.Now().Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if a.validSession(c.Value) {
		t.Fatal("expired session accepted")
	}
	if a.loginBlocked("new") {
		t.Fatal("new client blocked")
	}
	a.logins["old"] = loginAttempt{9, time.Now().Add(-time.Hour).Unix()}
	if a.loginBlocked("old") {
		t.Fatal("expired lockout")
	}
}

func TestConcurrentLoginThrottle(t *testing.T) {
	a := newTestApp(t)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Go(func() {
			<-start
			_, blocked := a.checkPassword("shared-client", "wrong")
			if !blocked {
				admitted.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 8 {
		t.Fatalf("admitted %d guesses, want 8", admitted.Load())
	}
	if valid, blocked := a.checkPassword("shared-client", "pw"); valid || !blocked {
		t.Fatal("blocked client authenticated")
	}
	if valid, blocked := a.checkPassword("other-client", "pw"); !valid || blocked {
		t.Fatal("independent client blocked")
	}
}

func TestHostIsolationTokenSecrecyAndEscapedOutput(t *testing.T) {
	a := newTestApp(t)
	var callsA, callsB atomic.Int32
	newAgent := func(counter *atomic.Int32, token string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("wrong token for host")
			}
			counter.Add(1)
			switch r.URL.Path {
			case "/v1/inventory":
				_ = json.NewEncoder(w).Encode(management.Inventory{Containers: []management.Container{{Name: "<script>alert(1)</script>", Project: "<script>project</script>"}}})
			case "/v1/jobs":
				if r.Method == "GET" {
					t.Error("dashboard requested removed operation history")
					http.Error(w, "history removed", http.StatusMethodNotAllowed)
				} else {
					w.WriteHeader(202)
					_ = json.NewEncoder(w).Encode(management.Job{ID: "job-1"})
				}
			default:
				_ = json.NewEncoder(w).Encode(management.Job{Output: "<script>evil</script>", Status: "failed"})
			}
		}))
	}
	ta, tb := strings.Repeat("a", 32), strings.Repeat("b", 32)
	sa, sb := newAgent(&callsA, ta), newAgent(&callsB, tb)
	defer sa.Close()
	defer sb.Close()
	a.cfg.Hosts = []Host{{ID: "a", Name: "A", URL: sa.URL, Token: ta, AllowHTTP: true}, {ID: "b", Name: "B", URL: sb.URL, Token: tb, AllowHTTP: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	c := session(t, a)
	r := httptest.NewRequest("GET", "https://docklight.local/docklight/?host=b&job=job-1", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("dashboard %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if callsA.Load() != 0 || callsB.Load() != 2 {
		t.Fatalf("host leakage %d/%d", callsA.Load(), callsB.Load())
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, ta) || strings.Contains(body, tb) || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("unescaped output or leaked secret")
	}
	if !strings.Contains(body, "&lt;script&gt;project&lt;/script&gt;") {
		t.Fatal("project heading missing or not escaped")
	}
	r = httptest.NewRequest("GET", "https://docklight.local/docklight/operation?host=b&job=job-1", nil)
	r.AddCookie(c)
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	var job management.Job
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil || job.Output != "<script>evil</script>" || w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || strings.Contains(w.Body.String(), tb) {
		t.Fatalf("invalid operation response: %d %s", w.Code, w.Body.String())
	}
	if callsA.Load() != 0 || callsB.Load() != 3 {
		t.Fatal("poll fetched inventory/history or wrong host")
	}
	for _, query := range []string{"job=job-1", "host=unknown&job=job-1", "host=b", "host=b&job=../other", "host=b&job=%3Cbad%3E"} {
		r = httptest.NewRequest("GET", "https://docklight.local/docklight/operation?"+query, nil)
		r.AddCookie(c)
		w = httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 400 || callsB.Load() != 3 || callsA.Load() != 0 {
			t.Fatalf("invalid poll reached host: %s HTTP %d", query, w.Code)
		}
	}
	values := url.Values{"host": {"b"}, "kind": {"container"}, "id": {"id"}, "operation": {"stop"}}
	r = httptest.NewRequest("GET", "https://docklight.local/docklight/inventory?host=b", nil)
	r.AddCookie(c)
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 200 || callsA.Load() != 0 || callsB.Load() != 4 || !strings.Contains(w.Body.String(), `data-inventory`) || strings.Contains(w.Body.String(), `class="topbar"`) || strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), tb) {
		t.Fatalf("unsafe inventory fragment: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `name="csrf_token" value="`+a.csrfToken(c.Value)+`"`) || !strings.Contains(w.Body.String(), `name="host" value="b"`) {
		t.Fatal("refreshed action forms lost CSRF or selected host")
	}
	for _, query := range []string{"", "host=unknown"} {
		r = httptest.NewRequest("GET", "https://docklight.local/docklight/inventory?"+query, nil)
		r.AddCookie(c)
		w = httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 400 || callsA.Load() != 0 || callsB.Load() != 4 {
			t.Fatal("inventory fell back to wrong host")
		}
	}
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, c, "/action", values))
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "host=b") {
		t.Fatalf("action %d %s", w.Code, w.Body.String())
	}
	for _, host := range []string{"", "unknown"} {
		values.Set("host", host)
		before := callsB.Load()
		w = httptest.NewRecorder()
		a.routes().ServeHTTP(w, formRequest(a, c, "/action", values))
		if w.Code != 400 || callsB.Load() != before {
			t.Fatal("unknown host fell back")
		}
	}
}

func TestDashboardReadsJobBeforeInventory(t *testing.T) {
	a := newTestApp(t)
	var finished atomic.Bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/jobs/test-job":
			finished.Store(true)
			_ = json.NewEncoder(w).Encode(management.Job{ID: "test-job", Status: "success"})
		case "/v1/inventory":
			state := "running"
			if finished.Load() {
				state = "exited"
			}
			_ = json.NewEncoder(w).Encode(management.Inventory{Containers: []management.Container{{Name: "stopped-container", State: state}}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer agent.Close()
	a.cfg.Hosts = []Host{{ID: "local", Name: "Local", URL: agent.URL, Token: strings.Repeat("t", 32), AllowHTTP: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://docklight.local/docklight/?host=local&job=test-job", nil)
	r.AddCookie(session(t, a))
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `class="badge badge-exited"`) {
		t.Fatalf("completed job rendered stale inventory: %d %s", w.Code, w.Body.String())
	}
}

func TestOperationProgressSurvivesHistoryRemoval(t *testing.T) {
	for _, status := range []string{"running", "updating", "success", "failed"} {
		t.Run(status, func(t *testing.T) {
			a := newTestApp(t)
			var calls atomic.Int32
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch r.URL.Path {
				case "/v1/inventory":
					_ = json.NewEncoder(w).Encode(management.Inventory{})
				case "/v1/jobs/test-job":
					_ = json.NewEncoder(w).Encode(management.Job{ID: "test-job", Status: status, Action: management.Action{Kind: "container", Operation: "build"}, Output: "<script>build output</script>"})
				default:
					t.Errorf("unexpected agent request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer agent.Close()
			a.cfg.Hosts = []Host{{ID: "local", Name: "Local", URL: agent.URL, Token: strings.Repeat("t", 32), AllowHTTP: true}}
			if err := configureHosts(a.cfg.Hosts); err != nil {
				t.Fatal(err)
			}
			cookie := session(t, a)
			r := httptest.NewRequest("GET", "https://docklight.local/docklight/?host=local&job=test-job", nil)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			body := w.Body.String()
			if w.Code != 200 || calls.Load() != 2 || !strings.Contains(body, `aria-label="Operation output"`) || !strings.Contains(body, "&lt;script&gt;build output&lt;/script&gt;") {
				t.Fatalf("operation result lost: %d %s", w.Code, body)
			}
			if strings.Contains(body, `http-equiv="refresh"`) || !strings.Contains(body, `data-status="`+status+`"`) || !strings.Contains(body, `data-operation-url="/docklight/operation?host=local&amp;job=test-job"`) {
				t.Fatalf("wrong async polling state: %s", status)
			}
			if strings.Contains(body, "activity") || strings.Contains(body, "Activity") {
				t.Fatal("activity feature still rendered")
			}
			if !strings.Contains(body, `data-operation-reopen hidden`) || !strings.Contains(body, `>Close output</a>`) {
				t.Fatal("new operation must show output with close control")
			}
			r = httptest.NewRequest("GET", "https://docklight.local/docklight/?host=local&job=test-job&output=hidden", nil)
			r.AddCookie(cookie)
			w = httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			body = w.Body.String()
			if w.Code != 200 || !strings.Contains(body, `data-status="`+status+`" hidden`) || strings.Contains(body, `data-operation-reopen hidden`) || !strings.Contains(body, `>Show output</a>`) {
				t.Fatalf("dismissed operation must remain reopenable: %s", body)
			}
		})
	}
}

func TestTLSAndRedirectsFailClosed(t *testing.T) {
	token := strings.Repeat("t", 32)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer secure.Close()
	hosts := []Host{{ID: "tls", Name: "TLS", URL: secure.URL, Token: token}}
	if err := configureHosts(hosts); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://ui.local", nil)
	var result any
	if err := hosts[0].request(r, "GET", "/v1/inventory", nil, &result); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secure.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	hosts[0].CAFile = ca
	if err := configureHosts(hosts); err != nil {
		t.Fatal(err)
	}
	if err := hosts[0].request(r, "GET", "/v1/inventory", nil, &result); err != nil {
		t.Fatal("configured CA was not trusted", err)
	}
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	hosts = []Host{{ID: "redirect", Name: "Redirect", URL: redirect.URL, Token: token, AllowHTTP: true}}
	if err := configureHosts(hosts); err != nil {
		t.Fatal(err)
	}
	if err := hosts[0].request(r, "GET", "/v1/inventory", nil, &result); err == nil || leaked.Load() {
		t.Fatal("redirect followed")
	}
	for _, u := range []string{"http://agent:8081", "https://user:pass@agent", "https://agent?token=secret", "https://agent/path"} {
		if err := configureHosts([]Host{{ID: "test", Name: "Test", URL: u, Token: token}}); err == nil {
			t.Fatalf("unsafe URL accepted: %s", u)
		}
	}
}

func TestRoutesRequireAuthenticationAndRemovedFeaturesUnavailable(t *testing.T) {
	a := newTestApp(t)
	for _, path := range []string{"/docklight/", "/docklight/action", "/docklight/operation?host=local&job=test-job", "/docklight/inventory?host=local"} {
		method := "GET"
		if strings.HasSuffix(path, "action") {
			method = "POST"
		}
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest(method, "https://docklight.local"+path, nil))
		if w.Code != 302 {
			t.Fatalf("unauthenticated route %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/stacks/new", "/containers/id/exec", "/usage/containers"} {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest("GET", "https://docklight.local/docklight"+path, nil))
		if w.Code != 404 {
			t.Fatalf("removed route remains %s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest("GET", "https://docklight.local/docklight", nil))
	if w.Code != 302 || w.Header().Get("Location") != "/docklight/" {
		t.Fatal("base redirect")
	}
}

func TestConfigurationValidation(t *testing.T) {
	t.Setenv("DB_PATH", "")
	t.Setenv("ADMIN_PASSWORD", "password")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 32))
	t.Setenv("BASE_URL", "/docklight")
	path := filepath.Join(t.TempDir(), "hosts.yml")
	t.Setenv("HOSTS_FILE", path)
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	valid := "hosts:\n  - id: local\n    name: Local\n    url: https://agent.example:8081\n    token: " + strings.Repeat("t", 32) + "\n"
	write(valid)
	cfg, err := loadConfig()
	if err != nil || len(cfg.Hosts) != 1 || cfg.Hosts[0].client == nil {
		t.Fatal("valid config", err)
	}
	if cfg.DBPath != "/data/sessions.db" {
		t.Fatalf("unexpected session database default: %s", cfg.DBPath)
	}
	for _, content := range []string{valid + "unknown: true\n", valid + "---\n{}", "hosts: []", strings.ReplaceAll(valid, "https://", "http://"), strings.ReplaceAll(valid, strings.Repeat("t", 32), "short")} {
		write(content)
		if _, err = loadConfig(); err == nil {
			t.Fatalf("invalid configuration accepted: %s", content)
		}
	}
	write(valid)
	for _, base := range []string{"/../path", "/a//b", "/a?token=x", "/a\\b"} {
		t.Setenv("BASE_URL", base)
		if _, err = loadConfig(); err == nil {
			t.Fatalf("unsafe base %s", base)
		}
	}
	t.Setenv("BASE_URL", "/")
	t.Setenv("SESSION_SECRET", "short")
	if _, err = loadConfig(); err == nil {
		t.Fatal("weak session secret accepted")
	}
}

func TestSessionDatabaseFailureFailsClosed(t *testing.T) {
	a := newTestApp(t)
	c := session(t, a)
	if _, err := a.db.Exec(`DROP TABLE sessions`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.login(w, formRequest(a, nil, "/login", url.Values{"password": {"pw"}}))
	if w.Code == 302 || !strings.Contains(w.Body.String(), "Unable to create session") {
		t.Fatal("database failure created session")
	}
	if a.validSession(c.Value) {
		t.Fatal("database failure accepted session")
	}
}
