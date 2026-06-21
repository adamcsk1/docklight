package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleRegistersPatternWithoutExplicitMethod(t *testing.T) {
	app := &App{cfg: Config{BaseURL: "/docklight"}}
	mux := http.NewServeMux()
	app.handle(mux, "/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), false)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://docklight.local/docklight/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("expected methodless route to serve, code=%d body=%q", rec.Code, rec.Body.String())
	}
}
