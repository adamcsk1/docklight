package main

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginCreatesSignedSessionAndRequireAuthRejectsBadRequests(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/docklight", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}

	values := url.Values{"username": {"admin"}, "password": {"pw"}}
	req := httptest.NewRequest(http.MethodPost, "https://docklight.local/docklight/login", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.login(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected login redirect, got %d", rec.Code)
	}
	var session *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "docklight_session" {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("expected session cookie")
	}
	if !session.HttpOnly || !session.Secure || session.Path != "/docklight" || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected session cookie flags: %#v", session)
	}
	if !app.validSession(session.Value) {
		t.Fatal("expected signed session to be valid")
	}
	if app.validSession(session.Value + "tamper") {
		t.Fatal("expected tampered session to be rejected")
	}

	var sessionRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessionRows); err != nil || sessionRows != 1 {
		t.Fatalf("expected one session row, got %d, %v", sessionRows, err)
	}

	called := false
	handler := app.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	post := httptest.NewRequest(http.MethodPost, "https://docklight.local/docklight/rescan", nil)
	post.AddCookie(session)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, post)
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("expected authenticated POST without origin to be rejected, code=%d called=%v", rec.Code, called)
	}

	csrfValues := url.Values{"csrf_token": {app.csrfToken(session.Value)}}
	post = httptest.NewRequest(http.MethodPost, "https://docklight.local/docklight/rescan", strings.NewReader(csrfValues.Encode()))
	post.Header.Set("Origin", "https://docklight.local")
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(session)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, post)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("expected same-origin POST to pass, code=%d called=%v", rec.Code, called)
	}
}

func TestEnsureSessionSchemaDropsLegacyPlainSessionTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, username TEXT NOT NULL, expires_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	app := &App{db: db, cfg: Config{SessionSecret: "secret"}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.tableHasColumn("sessions", "id_hash"); err != nil || !ok {
		t.Fatalf("expected migrated sessions.id_hash column, ok=%v err=%v", ok, err)
	}
	if ok, err := app.tableHasColumn("sessions", "id"); err != nil || ok {
		t.Fatalf("expected legacy sessions.id column to be removed, ok=%v err=%v", ok, err)
	}
}

func TestLoginReportsSessionInsertFailure(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE sessions`); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"username": {"admin"}, "password": {"pw"}}
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/login", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.login(rec, req)
	if rec.Code == http.StatusFound || !strings.Contains(rec.Body.String(), "Unable to create session") {
		t.Fatalf("expected rendered session error, code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthFailureLockoutLogoutAndExpiredSession(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/docklight", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/docklight/login", nil)
	rec := httptest.NewRecorder()
	app.loginPage(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Login") {
		t.Fatalf("expected login page, code=%d", rec.Code)
	}

	for i := 0; i < 8; i++ {
		values := url.Values{"username": {"admin"}, "password": {"bad"}}
		req = httptest.NewRequest(http.MethodPost, "http://docklight.local/docklight/login", strings.NewReader(values.Encode()))
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec = httptest.NewRecorder()
		app.login(rec, req)
	}
	values := url.Values{"username": {"admin"}, "password": {"pw"}}
	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/docklight/login", strings.NewReader(values.Encode()))
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	app.login(rec, req)
	if !strings.Contains(rec.Body.String(), "Too many failed login attempts") {
		t.Fatalf("expected lockout page, body=%s", rec.Body.String())
	}

	sid := "expired-session"
	if _, err := db.Exec(`INSERT INTO sessions (id_hash, username, expires_at) VALUES (?, 'admin', ?)`, app.sessionIDHash(sid), time.Now().Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if app.validSession(app.signSessionID(sid)) {
		t.Fatal("expired session should be invalid")
	}

	sid = "active-session"
	if _, err := db.Exec(`INSERT INTO sessions (id_hash, username, expires_at) VALUES (?, 'admin', ?)`, app.sessionIDHash(sid), time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/docklight/logout", nil)
	req.AddCookie(&http.Cookie{Name: "docklight_session", Value: app.signSessionID(sid)})
	rec = httptest.NewRecorder()
	app.logout(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/docklight/login" || app.validSession(app.signSessionID(sid)) {
		t.Fatalf("expected logout to remove session, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestRoutesBaseURLAuthAndActionHandlers(t *testing.T) {
	root := t.TempDir()
	mustWriteCompose(t, filepath.Join(root, "web"))
	app, db := newTestApp(t, Config{BaseURL: "/docklight", StacksRoot: root, SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	installInventoryRunner(app)
	if err := app.rescanStacks(); err != nil {
		t.Fatal(err)
	}

	handler := app.routes()
	req := httptest.NewRequest(http.MethodGet, "http://docklight.local/docklight", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/docklight/" {
		t.Fatalf("expected base redirect, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/docklight/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/docklight/login" {
		t.Fatalf("expected unauthenticated dashboard redirect, code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	sid := "route-session"
	if _, err := db.Exec(`INSERT INTO sessions (id_hash, username, expires_at) VALUES (?, 'admin', ?)`, app.sessionIDHash(sid), time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/docklight/", nil)
	req.AddCookie(&http.Cookie{Name: "docklight_session", Value: app.signSessionID(sid)})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "web") {
		t.Fatalf("expected authenticated dashboard, code=%d body=%s", rec.Code, rec.Body.String())
	}

}

func TestRenderFlashQueryBasePathAndAuthUtilityBranches(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/docklight", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewRecorder()
	seedReq := httptest.NewRequest(http.MethodGet, "https://docklight.local/docklight/", nil)
	app.setFlashCookie(seed, seedReq, "message", "from flash")
	req := httptest.NewRequest(http.MethodGet, "https://docklight.local/docklight//?message=query&error=query", nil)
	for _, cookie := range seed.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	app.render(rec, req, "settings.html", PageData{Title: "Settings", Config: app.cfg})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "from flash") || !strings.Contains(body, "query") {
		t.Fatalf("expected flash message and query error to render, code=%d body=%s", rec.Code, body)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("expected flash cookie to be cleared, header=%s", rec.Header().Get("Set-Cookie"))
	}

	req = httptest.NewRequest(http.MethodGet, "http://docklight.local/", nil)
	req.Header.Set("Referer", "http://docklight.local/stacks")
	if !validRequestOrigin(req) {
		t.Fatal("expected same-host referer to be valid")
	}
	req.RemoteAddr = "raw-client"
	if clientKey(req) != "raw-client" {
		t.Fatalf("expected raw remote addr client key, got %q", clientKey(req))
	}
	req.RemoteAddr = ""
	if clientKey(req) != "unknown" {
		t.Fatalf("expected unknown client key, got %q", clientKey(req))
	}
	app.logins = map[string]loginAttempt{"old": {Count: 9, FirstSeen: time.Now().Add(-20 * time.Minute).Unix()}}
	if app.loginBlocked("old") {
		t.Fatal("expired login lockout should clear")
	}
}

func TestFlashCookiesVerifyAndRejectTampering(t *testing.T) {
	app := &App{cfg: Config{BaseURL: "/docklight", SessionSecret: "secret"}}
	req := httptest.NewRequest(http.MethodGet, "https://docklight.local/docklight/", nil)
	rec := httptest.NewRecorder()
	app.setFlashCookie(rec, req, "message", strings.Repeat("x", maxFlashMessage+20))
	cookie := rec.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/docklight" {
		t.Fatalf("unexpected flash cookie flags: %#v", cookie)
	}
	req.AddCookie(cookie)
	key, message, ok := app.readFlashCookie(req)
	if !ok || key != "message" || !strings.Contains(message, "output truncated") {
		t.Fatalf("expected signed truncated flash, key=%q ok=%v message=%q", key, ok, message)
	}
	if _, ok := app.verifyFlash(cookie.Value + "tamper"); ok {
		t.Fatal("expected tampered flash to fail")
	}
}
