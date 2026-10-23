package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestActionRejectionsAndUncertainOutcomes(t *testing.T) {
	for _, status := range []int{202, 400, 401, 403, 404, 405, 409, 413, 500, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a := newTestApp(t)
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "private-token-diagnostic", status) }))
			defer agent.Close()
			a.cfg.Hosts = []Host{{ID: "local", Name: "Local", URL: agent.URL, Token: strings.Repeat("s", 32), AllowHTTP: true}}
			if err := configureHosts(a.cfg.Hosts); err != nil {
				t.Fatal(err)
			}
			cookie := session(t, a)
			values := url.Values{"host": {"local"}, "kind": {"container"}, "id": {"target"}, "operation": {"stop"}}
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, formRequest(a, cookie, "/action", values))
			body := w.Body.String()
			if w.Code != 502 || strings.Contains(body, "private-token") || strings.Contains(body, "outcome may be unknown") != (status >= 500 || status == 202) {
				t.Fatalf("misleading rejection: %d %s", w.Code, body)
			}
		})
	}
}

func TestActionTransportFailureKeepsUncertainWarning(t *testing.T) {
	a := newTestApp(t)
	agent := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	agent.Close()
	a.cfg.Hosts = []Host{{ID: "local", Name: "Local", URL: agent.URL, Token: strings.Repeat("s", 32), AllowHTTP: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, session(t, a), "/action", url.Values{"host": {"local"}, "kind": {"container"}, "id": {"target"}, "operation": {"stop"}}))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "outcome may be unknown") {
		t.Fatalf("transport failure misreported: %d %s", w.Code, w.Body.String())
	}
}

func TestActionValidationExplainsMissingConfirmation(t *testing.T) {
	a := newTestApp(t)
	a.cfg.Hosts = []Host{{ID: "local", Name: "Local"}}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, session(t, a), "/action", url.Values{"host": {"local"}, "kind": {"volume"}, "id": {"data"}, "operation": {"delete"}}))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "explicit confirmation required") || strings.Contains(w.Body.String(), "unknown") {
		t.Fatalf("missing validation: %d %s", w.Code, w.Body.String())
	}
}
