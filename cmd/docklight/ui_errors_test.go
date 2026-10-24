package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func requireDocklightError(t *testing.T, w *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	body := w.Body.String()
	if w.Code != status || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("wrong error response: HTTP %d, content type %q, body %s", w.Code, w.Header().Get("Content-Type"), body)
	}
	for _, marker := range []string{"<!doctype html>", `class="topbar"`, `class="alert alert-danger" role="alert"`, "Return to dashboard"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("error left Docklight UI; missing %q: %s", marker, body)
		}
	}
	if message != "" && !strings.Contains(body, message) {
		t.Fatalf("error explanation missing %q: %s", message, body)
	}
}

func TestBrowserErrorPagesAcrossRoutes(t *testing.T) {
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a := newTestApp(t)
			a.cfg.BaseURL = base
			a.cfg.Hosts = []Host{{ID: "main", Name: "Main"}}
			cookie := session(t, a)
			handler := a.routes()
			tests := []struct {
				name, method, path string
				form               url.Values
				status             int
			}{
				{name: "dashboard host", method: "GET", path: "/?host=unknown", status: 400},
				{name: "dashboard job", method: "GET", path: "/?host=main&job=..%2Fbad", status: 400},
				{name: "inventory", method: "GET", path: "/inventory", status: 400},
				{name: "operation", method: "GET", path: "/operation", status: 400},
				{name: "maintenance", method: "POST", path: "/maintenance", form: url.Values{"host": {"main"}, "confirmed": {"yes"}}, status: 400},
				{name: "action validation", method: "POST", path: "/action", form: url.Values{"host": {"main"}, "kind": {"volume"}, "id": {"data"}, "operation": {"delete"}}, status: 400},
				{name: "CSRF", method: "POST", path: "/logout", form: url.Values{}, status: 403},
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					var r *http.Request
					if tc.method == "POST" {
						r = formRequest(a, cookie, tc.path, tc.form)
						r.URL.Path = a.withBase(tc.path)
						if tc.name == "CSRF" {
							if err := r.ParseForm(); err != nil {
								t.Fatal(err)
							}
							r.Form.Set("csrf_token", "invalid")
						}
					} else {
						r = httptest.NewRequest(tc.method, "https://docklight.local"+a.withBase(tc.path), nil)
						r.AddCookie(cookie)
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					requireDocklightError(t, w, tc.status, "")
				})
			}
		})
	}
}

func TestBrowserErrorMiddlewarePreservesStatusAndEscapesFutureErrors(t *testing.T) {
	for _, status := range []int{400, 403, 404, 409, 500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a := newTestApp(t)
			a.cfg.Hosts = []Host{{ID: "main", Name: "Main"}}
			handler := a.browserErrors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "3")
				http.Error(w, "failure <script>alert(1)</script>", status)
			}))
			r := httptest.NewRequest("GET", "https://docklight.local/docklight/?host=main", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			requireDocklightError(t, w, status, "failure &lt;script&gt;alert(1)&lt;/script&gt;")
			if strings.Contains(w.Body.String(), "<script") || w.Header().Get("Retry-After") != "3" || !strings.Contains(w.Body.String(), `href="/docklight/?host=main"`) {
				t.Fatalf("unsafe error page or lost recovery metadata: %s %v", w.Body.String(), w.Header())
			}
		})
	}
}

func TestBrowserErrorMiddlewareLeavesSuccessfulResponsesUntouched(t *testing.T) {
	a := newTestApp(t)
	handler := a.browserErrors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/operation", nil))
	if w.Code != 202 || w.Body.String() != `{"ok":true}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("successful response changed: %d %s", w.Code, w.Body.String())
	}
}
