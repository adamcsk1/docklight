package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckHealthHTTPAndTLS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected health request: %s %v", r.URL.Path, r.Header)
				}
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = w.Write([]byte("ok"))
			})
			var server *httptest.Server
			if secure {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			if err := checkHealth(server.URL+"/healthz", secure); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHealthURLUsesEffectiveListenAddress(t *testing.T) {
	for _, test := range []struct{ addr, want string }{
		{":8081", "https://localhost:8081/healthz"},
		{"127.0.0.1:9091", "https://127.0.0.1:9091/healthz"},
		{"0.0.0.0:9091", "https://localhost:9091/healthz"},
		{"[::1]:9091", "https://[::1]:9091/healthz"},
		{"[::]:9091", "https://localhost:9091/healthz"},
	} {
		got, err := healthURL("https", test.addr)
		if err != nil || got != test.want {
			t.Errorf("healthURL(%q): %q, %v; want %q", test.addr, got, err, test.want)
		}
	}
	for _, addr := range []string{"", "localhost", ":8081:9091", ":"} {
		if url, err := healthURL("https", addr); err == nil {
			t.Errorf("invalid ADDR %q produced %q", addr, url)
		}
	}
}

func TestCheckHealthRejectsWrongResponse(t *testing.T) {
	for _, response := range []struct {
		status int
		body   string
	}{{http.StatusNoContent, ""}, {http.StatusOK, "ok\n"}, {http.StatusOK, "ok-secret"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(response.status)
			_, _ = w.Write([]byte(response.body))
		}))
		if err := checkHealth(server.URL+"/healthz", false); err == nil {
			t.Errorf("accepted HTTP %d body %q", response.status, response.body)
		}
		server.Close()
	}
}
