package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"docklight/internal/buildinfo"
	"docklight/internal/management"
)

func TestBuildCommitsFollowSelectedHost(t *testing.T) {
	a := newTestApp(t)
	old := buildinfo.Commit
	buildinfo.Commit = strings.Repeat("c", 40)
	t.Cleanup(func() { buildinfo.Commit = old })
	for _, id := range []string{"a", "b", "old", "offline", "escaped"} {
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id == "offline" {
				http.Error(w, "offline", 502)
				return
			}
			if id == "old" {
				_, _ = w.Write([]byte(`{"Containers":[],"Resources":[]}`))
				return
			}
			commit := strings.Repeat(id, 40)
			if id == "escaped" {
				commit = `<script>"&`
			}
			_ = json.NewEncoder(w).Encode(management.Inventory{Commit: commit})
		}))
		t.Cleanup(agent.Close)
		a.cfg.Hosts = append(a.cfg.Hosts, Host{ID: id, Name: id, URL: agent.URL, Token: strings.Repeat("t", 32), AllowHTTP: true})
	}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	cookie := session(t, a)
	for _, base := range []string{"/", "/docklight"} {
		a.cfg.BaseURL = base
		for _, id := range []string{"a", "b", "old", "offline", "escaped"} {
			want := strings.Repeat(id, 7)
			if id == "old" {
				want = "unknown"
			}
			if id == "offline" {
				want = "unavailable"
			}
			if id == "escaped" {
				want = "&lt;script"
			}
			for _, path := range []string{"/", "/inventory"} {
				r := httptest.NewRequest("GET", "https://docklight.local"+a.withBase(path)+"?host="+id, nil)
				r.AddCookie(cookie)
				w := httptest.NewRecorder()
				a.routes().ServeHTTP(w, r)
				body := w.Body.String()
				if id == "offline" && path == "/inventory" {
					if w.Code != 502 {
						t.Fatal("failed inventory accepted")
					}
					continue
				}
				if w.Code != 200 || !strings.Contains(body, `data-ui-commit="`+buildinfo.Commit+`"`) || strings.Contains(body, "<script>") {
					t.Fatalf("unsafe/missing build metadata for %s: %s", id, body)
				}
				if path == "/" && (!strings.Contains(body, ">"+want+"</span>") || !strings.Contains(body, `title="`+buildinfo.Commit+`"`)) {
					t.Fatalf("wrong build display for %s: %s", id, body)
				}
				if id == "a" || id == "b" {
					if !strings.Contains(body, `data-agent-commit="`+strings.Repeat(id, 40)+`"`) {
						t.Fatal("agent commit not scoped to selected host")
					}
				}
			}
		}
	}
}

func TestShortCommit(t *testing.T) {
	for value, want := range map[string]string{"": "unknown", "unknown": "unknown", "unavailable": "unavailable", "abcdef123456": "abcdef1", "abc": "abc"} {
		if got := shortCommit(value); got != want {
			t.Fatalf("shortCommit(%q)=%q, want %q", value, got, want)
		}
	}
}
