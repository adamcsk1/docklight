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
	if _, err := a.db.Exec(`INSERT INTO sessions(id_hash,username,expires_at) VALUES(?,'admin',?)`, a.sessionIDHash(id), time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
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

func TestCSPAllowsOnlyReportedInlineStyle(t *testing.T) {
	a := newTestApp(t)
	for _, base := range []string{"/", "/docklight"} {
		a.cfg.BaseURL = base
		for _, path := range []string{"/login", "/static/manage.css"} {
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, httptest.NewRequest("GET", "https://docklight.local"+a.withBase(path), nil))
			if w.Code != http.StatusOK {
				t.Fatalf("%s: HTTP %d", path, w.Code)
			}
			policy := w.Header().Get("Content-Security-Policy")
			want := "default-src 'none'; style-src 'self' 'unsafe-hashes' 'sha256-vKC/3e+58wt3AEEnUImhhkZlNJYe2iGGM4zT9jQL6x0='; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
			if policy != want {
				t.Fatalf("unexpected CSP: %q", policy)
			}
			if strings.Contains(policy, "'unsafe-inline'") || strings.Contains(policy, "'unsafe-eval'") {
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
				_ = json.NewEncoder(w).Encode(management.Inventory{Containers: []management.Container{{Name: "<script>alert(1)</script>"}}})
			case "/v1/jobs":
				if r.Method == "GET" {
					_, _ = io.WriteString(w, "[]")
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
	if callsA.Load() != 0 || callsB.Load() != 3 {
		t.Fatalf("host leakage %d/%d", callsA.Load(), callsB.Load())
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, ta) || strings.Contains(body, tb) || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("unescaped output or leaked secret")
	}
	values := url.Values{"host": {"b"}, "kind": {"container"}, "id": {"id"}, "operation": {"stop"}}
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

func TestLegacyMigrationPreservesDataAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE stacks(name TEXT); INSERT INTO stacks VALUES('important'); CREATE TABLE activity_log(id INTEGER PRIMARY KEY,stack_name TEXT,action TEXT,status TEXT,output TEXT,created_at TEXT); INSERT INTO activity_log VALUES(1,'old','upgrade','success','history',CURRENT_TIMESTAMP); CREATE TABLE sessions(id TEXT PRIMARY KEY,username TEXT,expires_at TEXT);`); err != nil {
		t.Fatal(err)
	}
	a := &App{db: db, cfg: Config{DBPath: path}}
	if err = a.initDB(); err != nil {
		t.Fatal(err)
	}
	var name, output string
	if err = db.QueryRow(`SELECT name FROM stacks`).Scan(&name); err != nil || name != "important" {
		t.Fatal("stack metadata lost", err)
	}
	if err = db.QueryRow(`SELECT output FROM activity_log`).Scan(&output); err != nil || output != "history" {
		t.Fatal("history lost", err)
	}
	if _, err = os.Stat(path + ".pre-agent.bak"); err != nil {
		t.Fatal("no migration backup", err)
	}
	if _, err = db.Exec(`INSERT INTO sessions(id_hash,username,expires_at) VALUES('hash','admin','future')`); err != nil {
		t.Fatal("session migration", err)
	}
	if err = a.initDB(); err != nil {
		t.Fatal("migration not idempotent", err)
	}
}

func TestRoutesRequireAuthenticationAndRemovedFeaturesUnavailable(t *testing.T) {
	a := newTestApp(t)
	for _, path := range []string{"/docklight/", "/docklight/action"} {
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
	t.Setenv("ADMIN_USER", "")
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

func TestSessionDatabaseFailureAndLegacyIdentityFailClosed(t *testing.T) {
	a := newTestApp(t)
	c := session(t, a)
	if _, err := a.db.Exec(`UPDATE sessions SET username='legacy-user'`); err != nil {
		t.Fatal(err)
	}
	if a.validSession(c.Value) {
		t.Fatal("legacy named-user session accepted")
	}
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
