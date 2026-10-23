package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (app *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("docklight_session"); err == nil && app.validSession(cookie.Value) {
		http.Redirect(w, r, app.withBase("/"), http.StatusFound)
		return
	}
	app.render(w, r, "login.html", PageData{Title: "Login"})
}

func (app *App) login(w http.ResponseWriter, r *http.Request) {
	remote := clientKey(r)
	if app.loginBlocked(remote) {
		app.render(w, r, "login.html", PageData{Title: "Login", Error: "Too many failed login attempts. Try again later."})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(app.cfg.AdminPassword)) != 1 {
		app.recordLoginFailure(remote)
		log.Printf("failed admin login from %q", remote)
		app.render(w, r, "login.html", PageData{Title: "Login", Error: "Invalid password"})
		return
	}
	app.clearLoginFailures(remote)
	sid := uuid.NewString()
	expires := time.Now().Add(24 * time.Hour)
	if _, err := app.db.Exec(`INSERT INTO sessions (id_hash, expires_at) VALUES (?, ?)`, app.sessionIDHash(sid), expires.Format(time.RFC3339)); err != nil {
		app.render(w, r, "login.html", PageData{Title: "Login", Error: "Unable to create session"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "docklight_session", Value: app.signSessionID(sid), Path: app.cookiePath(), Expires: expires, HttpOnly: true, Secure: app.cookieSecure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, app.withBase("/"), http.StatusFound)
}

func (app *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("docklight_session"); err == nil {
		if sid, ok := app.verifySessionCookie(cookie.Value); ok {
			if _, err := app.db.Exec(`DELETE FROM sessions WHERE id_hash = ?`, app.sessionIDHash(sid)); err != nil {
				log.Printf("session revocation failed: %v", err)
				http.Error(w, "Unable to revoke session. Logout failed; retry when storage is available.", http.StatusServiceUnavailable)
				return
			}
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "docklight_session", Value: "", Path: app.cookiePath(), MaxAge: -1, HttpOnly: true, Secure: app.cookieSecure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, app.withBase("/login"), http.StatusFound)
}

func (app *App) cookieSecure(r *http.Request) bool {
	if app.cfg.CookieSecure || r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (app *App) cookiePath() string {
	if app.cfg.BaseURL == "/" {
		return "/"
	}
	return app.cfg.BaseURL
}

func (app *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("docklight_session")
		if err != nil || !app.validSession(cookie.Value) {
			http.Redirect(w, r, app.withBase("/login"), http.StatusFound)
			return
		}
		if requestHasSideEffects(r) && !validRequestOrigin(r) {
			http.Error(w, "bad request origin", http.StatusForbidden)
			return
		}
		if requestHasSideEffects(r) && !app.validCSRFToken(cookie.Value, r) {
			http.Error(w, "bad csrf token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (app *App) csrfToken(sessionCookie string) string {
	mac := hmac.New(sha256.New, []byte(app.cfg.SessionSecret))
	mac.Write([]byte("csrf:"))
	mac.Write([]byte(sessionCookie))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (app *App) validCSRFToken(sessionCookie string, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	provided := r.FormValue("csrf_token")
	expected := app.csrfToken(sessionCookie)
	return provided != "" && hmac.Equal([]byte(provided), []byte(expected))
}

func requestHasSideEffects(r *http.Request) bool {
	return r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
}

func validRequestOrigin(r *http.Request) bool {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	origin := r.Header.Get("Origin")
	if origin != "" {
		parsed, err := url.Parse(origin)
		return err == nil && parsed.Scheme == scheme && parsed.User == nil && sameHost(parsed.Host, r.Host)
	}
	referer := r.Header.Get("Referer")
	if referer == "" {
		return false
	}
	parsed, err := url.Parse(referer)
	return err == nil && parsed.Scheme == scheme && parsed.User == nil && sameHost(parsed.Host, r.Host)
}

func sameHost(a, b string) bool {
	return strings.EqualFold(a, b)
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

func (app *App) loginBlocked(key string) bool {
	app.loginMu.Lock()
	defer app.loginMu.Unlock()
	for key, attempt := range app.logins {
		if time.Since(time.Unix(attempt.FirstSeen, 0)) > 15*time.Minute {
			delete(app.logins, key)
		}
	}
	attempt, ok := app.logins[key]
	if !ok {
		return len(app.logins) >= 4096
	}
	if time.Since(time.Unix(attempt.FirstSeen, 0)) > 15*time.Minute {
		delete(app.logins, key)
		return false
	}
	return attempt.Count >= 8
}

func (app *App) recordLoginFailure(key string) {
	app.loginMu.Lock()
	defer app.loginMu.Unlock()
	if app.logins == nil {
		app.logins = make(map[string]loginAttempt)
	}
	for key, attempt := range app.logins {
		if time.Since(time.Unix(attempt.FirstSeen, 0)) > 15*time.Minute {
			delete(app.logins, key)
		}
	}
	if _, exists := app.logins[key]; len(app.logins) >= 4096 && !exists {
		return
	}
	attempt := app.logins[key]
	if attempt.FirstSeen == 0 || time.Since(time.Unix(attempt.FirstSeen, 0)) > 15*time.Minute {
		attempt = loginAttempt{FirstSeen: time.Now().Unix()}
	}
	attempt.Count++
	app.logins[key] = attempt
}

func (app *App) clearLoginFailures(key string) {
	app.loginMu.Lock()
	defer app.loginMu.Unlock()
	delete(app.logins, key)
}

func (app *App) validSession(id string) bool {
	sid, ok := app.verifySessionCookie(id)
	if !ok {
		return false
	}
	var expiresRaw string
	idHash := app.sessionIDHash(sid)
	err := app.db.QueryRow(`SELECT expires_at FROM sessions WHERE id_hash = ?`, idHash).Scan(&expiresRaw)
	if err != nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil || time.Now().After(expires) {
		_, _ = app.db.Exec(`DELETE FROM sessions WHERE id_hash = ?`, idHash)
		return false
	}
	return true
}

func (app *App) signSessionID(id string) string {
	mac := hmac.New(sha256.New, []byte(app.cfg.SessionSecret))
	mac.Write([]byte(id))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return id + "." + sig
}

func (app *App) verifySessionCookie(value string) (string, bool) {
	id, sig, ok := strings.Cut(value, ".")
	if !ok || id == "" || sig == "" {
		return "", false
	}
	expected := app.signSessionID(id)
	if subtle.ConstantTimeCompare([]byte(value), []byte(expected)) != 1 {
		return "", false
	}
	return id, true
}

func (app *App) sessionIDHash(id string) string {
	mac := hmac.New(sha256.New, []byte(app.cfg.SessionSecret))
	mac.Write([]byte("db:"))
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil))
}
