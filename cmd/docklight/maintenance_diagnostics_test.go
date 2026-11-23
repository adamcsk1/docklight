package main

import (
	"context"
	"html"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMaintenanceTargetDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, state, health, service, targetError string
		want                                      []string
	}{
		{name: "unhealthy", state: "running", health: "unhealthy", service: "agent", want: []string{`health="unhealthy" (expected healthy)`}},
		{name: "starting", state: "running", health: "starting", service: "agent", want: []string{`health="starting" (expected healthy)`}},
		{name: "exited", state: "exited", service: "agent", want: []string{`container state="exited" (expected running)`}},
		{name: "missing-state", service: "agent", want: []string{`container state="" (expected running)`}},
		{name: "missing-service", state: "running", want: []string{"Compose service metadata missing"}},
		{name: "agent-error", state: "running", service: "agent", targetError: "Update unavailable: check Compose files", want: []string{"Update unavailable: check Compose files"}},
		{name: "combined", state: "restarting", health: "unhealthy", targetError: "<script>agent diagnostic</script>", want: []string{"<script>agent diagnostic</script>", "Compose service metadata missing", `container state="restarting" (expected running)`, `health="unhealthy" (expected healthy)`}},
		{name: "healthy", state: "running", health: "healthy", service: "agent"},
		{name: "no-healthcheck", state: "running", service: "agent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			target := &agents[1].info.Targets[0]
			target.State, target.Health, target.Service, target.Error = test.state, test.health, test.service, test.targetError
			v := a.checkMaster(context.Background(), true)
			run, err := a.preflight(v, nil)
			if len(test.want) == 0 {
				if err != nil || run == nil {
					t.Fatalf("valid target blocked: %v", err)
				}
				return
			}
			if err == nil || run != nil {
				t.Fatalf("unsafe target accepted: %+v", run)
			}
			if !strings.Contains(err.Error(), "remote/agent: update target unavailable or unhealthy: ") {
				t.Fatalf("missing target identity: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing %q: %v", want, err)
				}
			}
			// Exercise the same authenticated form and styled error page as the UI.
			r := formRequest(a, session(t, a), "/maintenance", url.Values{"host": {"main"}, "confirmed": {"yes"}, "selected": {"main", "remote"}})
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			if w.Code != 409 {
				t.Fatalf("unsafe update returned %d: %s", w.Code, w.Body.String())
			}
			for _, want := range test.want {
				if !strings.Contains(w.Body.String(), html.EscapeString(want)) {
					t.Fatalf("error page lost %q: %s", want, w.Body.String())
				}
			}
			if strings.Contains(w.Body.String(), "<script>agent diagnostic</script>") {
				t.Fatal("agent diagnostic rendered as HTML")
			}
			for _, agent := range agents {
				if agent.posts != 0 {
					t.Fatal("blocked preflight submitted an update")
				}
			}
			if persisted, err := a.loadMaintenance(); err != nil || persisted != nil {
				t.Fatalf("blocked preflight persisted a run: %+v %v", persisted, err)
			}
		})
	}
}
