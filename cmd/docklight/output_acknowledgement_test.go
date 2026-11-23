package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"docklight/internal/management"
)

func TestOutputAcknowledgement(t *testing.T) {
	a := newTestApp(t)
	job := management.Job{ID: "job-1", Status: "running"}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/inventory" {
			_ = json.NewEncoder(w).Encode(management.Inventory{})
		} else {
			_ = json.NewEncoder(w).Encode(job)
		}
	}))
	defer agent.Close()
	a.cfg.Hosts = []Host{{ID: "local", Name: "Local", URL: agent.URL, Token: strings.Repeat("t", 32), AllowHTTP: true}, {ID: "other", Name: "Other", URL: agent.URL, Token: strings.Repeat("t", 32), AllowHTTP: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	cookie := session(t, a)
	check := func(query string, hidden bool) {
		t.Helper()
		r := httptest.NewRequest("GET", "https://docklight.local/docklight/?"+query, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), `data-operation-reopen hidden`) == hidden {
			t.Fatalf("query=%s hidden=%v: %d %s", query, hidden, w.Code, w.Body.String())
		}
	}
	check("host=local", false)
	for range 2 {
		r := formRequest(a, cookie, "/operation/acknowledge", url.Values{"host": {"local"}, "job": {job.ID}, "view": {"cleanup"}})
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 303 {
			t.Fatalf("acknowledge: %d %s", w.Code, w.Body.String())
		}
		location, _ := url.Parse(w.Header().Get("Location"))
		if location.Query().Get("view") != "cleanup" || location.Query().Get("output") != "hidden" {
			t.Fatal(location)
		}
	}
	r := formRequest(a, cookie, "/operation/acknowledge", url.Values{"host": {"local"}, "job": {job.ID}})
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("asynchronous acknowledgement: %d", w.Code)
	}
	check("host=local", true)
	check("host=local&view=cleanup", true)
	check("host=other", false)
	check("host=local&job=job-1&output=shown", false)
	// Inspecting output does not revoke the durable acknowledgement.
	check("host=local", true)
	job.Status = "success"
	check("host=local", true)
	// A different session/browser shares the single-admin acknowledgement.
	if _, err := a.db.Exec(`INSERT INTO sessions SELECT ?,expires_at FROM sessions LIMIT 1`, a.sessionIDHash("other-session")); err != nil {
		t.Fatal(err)
	}
	cookie = &http.Cookie{Name: "docklight_session", Value: a.signSessionID("other-session")}
	check("host=local", true)
	job.ID = "job-2"
	check("host=local", false)
	job.ID = "job-1"
	a.cfg.Hosts[0].URL += "/replacement"
	check("host=local", false)
}

func TestOutputAcknowledgementValidation(t *testing.T) {
	a := newTestApp(t)
	a.cfg.Hosts = []Host{{ID: "local", URL: "http://agent"}}
	cookie := session(t, a)
	for _, test := range []struct {
		name   string
		values url.Values
		status int
	}{
		{"host", url.Values{"host": {"missing"}, "job": {"job-1"}}, 400},
		{"job", url.Values{"job": {"../bad"}}, 400},
		{"view", url.Values{"job": {"job-1"}, "view": {"bad"}}, 400},
		{"csrf", url.Values{"job": {"job-1"}, "csrf_token": {"bad"}}, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := formRequest(a, cookie, "/operation/acknowledge", test.values)
			if test.name == "csrf" {
				r.ParseForm()
				r.PostForm.Set("csrf_token", "bad")
				r.Form.Set("csrf_token", "bad")
			}
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, mode := range []string{"origin", "session", "get"} {
		r := formRequest(a, cookie, "/operation/acknowledge", url.Values{"job": {"job-1"}})
		want := 403
		switch mode {
		case "origin":
			r.Header.Set("Origin", "https://attacker.invalid")
		case "session":
			r.Header.Del("Cookie")
			want = 302
		case "get":
			r.Method = "GET"
			want = 405
		}
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d", mode, w.Code)
		}
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM output_acknowledgements`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected request changed acknowledgements: %d %v", count, err)
	}
	if _, err := a.db.Exec(`DROP TABLE output_acknowledgements`); err != nil {
		t.Fatal(err)
	}
	r := formRequest(a, cookie, "/operation/acknowledge", url.Values{"job": {"job-1"}})
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 500 {
		t.Fatalf("storage failure hidden: %d", w.Code)
	}
}
