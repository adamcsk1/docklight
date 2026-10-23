package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCleanupFormsAndSubmission(t *testing.T) {
	for _, base := range []string{"/", "/docklight"} {
		a, actions := presentationApp(t)
		a.cfg.BaseURL = base
		cookie := session(t, a)
		for _, kind := range []string{"image", "volume", "network"} {
			for _, invalid := range []string{"confirmation", "host", "scope", "target", "csrf"} {
				values := url.Values{"host": {"pi"}, "kind": {kind}, "id": {"unused"}, "operation": {"delete-unused"}, "confirmed": {"yes"}}
				switch invalid {
				case "confirmation":
					values.Del("confirmed")
				case "host":
					values.Del("host")
				case "scope":
					values.Set("scope", "stack")
				case "target":
					values.Set("id", "other")
				}
				r := formRequest(a, cookie, "/action", values)
				r.URL.Path = a.withBase("/action")
				if invalid == "csrf" {
					r.Header.Set("Origin", "https://evil.example")
				}
				w := httptest.NewRecorder()
				a.routes().ServeHTTP(w, r)
				want := 400
				if invalid == "csrf" {
					want = 403
				}
				if w.Code != want || len(actions) != 0 {
					t.Fatalf("%s/%s accepted: %d %s", kind, invalid, w.Code, w.Body.String())
				}
			}
			w := httptest.NewRecorder()
			r := formRequest(a, cookie, "/action", url.Values{"host": {"pi"}, "kind": {kind}, "id": {"unused"}, "operation": {"delete-unused"}, "confirmed": {"yes"}})
			r.URL.Path = a.withBase("/action")
			a.routes().ServeHTTP(w, r)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != a.withBase("/")+"?host=pi&job=presentation-job" {
				t.Fatalf("cleanup redirect: %d %s", w.Code, w.Body.String())
			}
			got := <-actions
			if got.Kind != kind || got.Operation != "delete-unused" || got.ID != "unused" || !got.Confirmed || got.Scope != "" {
				t.Fatalf("wrong payload: %+v", got)
			}
		}
		r := httptest.NewRequest("GET", "https://docklight.local"+a.withBase("/inventory")+"?host=pi", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != 200 || strings.Count(body, `value="delete-unused"`) != 3 || strings.Count(body, "1 unused, unprotected") != 3 || !strings.Contains(body, "Permanently delete all unused volumes and their data") {
			t.Fatalf("missing cleanup controls: %d %s", w.Code, body)
		}
	}
}
