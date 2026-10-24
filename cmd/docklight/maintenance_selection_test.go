package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"docklight/internal/management"
	"github.com/google/uuid"
)

func submitSelected(t *testing.T, a *App, selected []string, skip bool) *httptest.ResponseRecorder {
	t.Helper()
	values := url.Values{"host": {"main"}, "confirmed": {"yes"}, "selected": selected}
	if skip {
		values.Set("skip_offline", "yes")
	}
	w := httptest.NewRecorder()
	a.db.Exec(`DELETE FROM sessions WHERE id_hash=?`, a.sessionIDHash("test-session"))
	a.routes().ServeHTTP(w, formRequest(a, session(t, a), "/maintenance", values))
	return w
}

func TestMaintenanceSelectionValidation(t *testing.T) {
	for _, selected := range [][]string{nil, {}, {"unknown"}, {"main", "main"}, {""}} {
		a, _, _ := maintenanceApp(t)
		w := submitSelected(t, a, selected, false)
		if w.Code != 400 {
			t.Fatalf("accepted %v: %d %s", selected, w.Code, w.Body.String())
		}
		if run, _ := a.loadMaintenance(); run != nil {
			t.Fatal("invalid selection persisted")
		}
	}
}

func TestMaintenanceSelectedSubset(t *testing.T) {
	for _, scenario := range []string{"master-deselected", "unselected-offline", "unselected-unhealthy", "unselected-old-agent"} {
		t.Run(scenario, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			selected, excluded := "main", 1
			if scenario == "master-deselected" {
				selected, excluded = "remote", 0
				agents[0].info.Targets = nil // Coordination does not require update discovery or a final UI.
				agents[0].info.Busy = true
			} else if scenario == "unselected-offline" {
				maintenanceTransportOffline(a, 1)(true)
			} else if scenario == "unselected-unhealthy" {
				agents[1].info.Targets[0].Health = "unhealthy"
			} else {
				agents[1].info.Version = 0
			}
			transport := a.cfg.Hosts[excluded].client.Transport
			a.cfg.Hosts[excluded].client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("discovery") == "true" {
					t.Error("unselected host discovery requested")
				}
				return transport.RoundTrip(r)
			})
			w := submitSelected(t, a, []string{selected}, false)
			if w.Code != 303 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			run, _ := a.loadMaintenance()
			if strings.Join(run.Selected, ",") != selected || len(run.Hosts) != 1 || run.Hosts[0].ID != selected {
				t.Fatalf("wrong scope: %+v", run)
			}
			restarted := &App{cfg: a.cfg, db: a.db}
			if err := restarted.initDB(); err != nil {
				t.Fatal(err)
			}
			run = tickMaintenance(t, restarted, 3)
			if run.Status != "success" || agents[excluded].posts != 0 || agents[excluded].info.State.Run != "" {
				t.Fatalf("subset failed: %+v", run)
			}
			if agents[1-excluded].posts != 2 {
				t.Fatal("selected updates missing")
			}
		})
	}
}

func TestMaintenanceSubsetKeepsCoordinationSafety(t *testing.T) {
	for _, scenario := range []string{"master-offline", "conflict", "remembered-conflict", "master-changed"} {
		t.Run(scenario, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			switch scenario {
			case "master-offline":
				maintenanceTransportOffline(a, 0)(true)
			case "conflict":
				agents[0].info.State.Owner = management.MasterClaim{Coordinator: uuid.NewString(), Master: "other", Name: "Other"}
			case "remembered-conflict":
				a.maintenanceValue("conflict", "remembered conflict")
				maintenanceTransportOffline(a, 1)(true)
			}
			selected := []string{"remote"}
			if scenario == "remembered-conflict" {
				selected = []string{"main"}
			}
			w := submitSelected(t, a, selected, false)
			if scenario != "master-changed" {
				if w.Code != 409 {
					t.Fatalf("unsafe start: %d %s", w.Code, w.Body.String())
				}
			} else {
				if w.Code != 303 {
					t.Fatal(w.Body.String())
				}
				agents[0].info.HostID = "replacement"
				agents[0].info.State.Owner = management.MasterClaim{}
				run := tickMaintenance(t, a, 1)
				if run.Status != "attention" {
					t.Fatalf("master change accepted: %+v", run)
				}
			}
			if agents[0].posts+agents[1].posts != 0 {
				t.Fatal("unsafe updates")
			}
		})
	}
}

func TestMaintenanceSelectionRetryAndLegacy(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	agents[1].fail = true
	if w := submitSelected(t, a, []string{"remote"}, false); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	run := tickMaintenance(t, a, 3)
	if run.Status != "attention" {
		t.Fatal(run.Status)
	}
	if w := submitSelected(t, a, []string{"main", "remote"}, false); w.Code != 409 {
		t.Fatal("retry expanded selection")
	}
	agents[1].fail = false
	if w := submitSelected(t, a, []string{"remote"}, false); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	run = tickMaintenance(t, a, 3)
	if run.Status != "success" || strings.Join(run.Selected, ",") != "remote" || agents[0].posts != 0 {
		t.Fatalf("retry scope: %+v", run)
	}
	// A persisted pre-feature run has no Selected field and retains all-host behavior.
	legacy := prepareMaintenance(t, a)
	if legacy.Selected != nil {
		t.Fatal("legacy fixture has selection")
	}
	legacy.Status = "attention"
	a.saveRun(legacy)
	if w := submitSelected(t, a, []string{"remote"}, false); w.Code != 409 {
		t.Fatal("legacy scope changed")
	}
	if w := submitSelected(t, a, []string{"main", "remote"}, false); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	run = tickMaintenance(t, a, 5)
	if run.Status != "success" || run.Selected != nil || agents[0].posts != 2 {
		t.Fatalf("legacy lost: %+v", run)
	}
}

func TestMaintenanceIdleHostStatusAndBusy(t *testing.T) {
	a, _, _ := maintenanceApp(t)
	maintenanceTransportOffline(a, 1)(true)
	for _, busy := range []bool{false, true} {
		if busy {
			a.maintenanceMu.Lock()
		}
		d := PageData{HostID: "main"}
		r := httptest.NewRequest("GET", "/docklight/maintenance?host=main", nil)
		a.populateMaintenance(r, &d)
		if busy {
			a.maintenanceMu.Unlock()
		}
		if len(d.MaintenanceHosts) != 2 || d.Maintenance != nil {
			t.Fatalf("missing idle hosts: %+v", d)
		}
		for _, h := range d.MaintenanceHosts {
			if !h.Checked || h.Frozen || (h.Online != (!busy && h.ID == "main")) {
				t.Fatalf("wrong host state: %+v", h)
			}
		}
		w := httptest.NewRecorder()
		a.render(w, r, "maintenance", d)
		body := w.Body.String()
		if strings.Contains(body, `<p role="status">`) || strings.Count(body, `name="selected"`) != 2 || !strings.Contains(body, "Offline / Unavailable") {
			t.Fatal(body)
		}
	}
}

func TestMaintenanceSelectedSkipExcludesUnselectedOffline(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	other := a.cfg.Hosts[1]
	other.ID, other.Name = "other", "other"
	a.cfg.Hosts = append(a.cfg.Hosts, other)
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	maintenanceTransportOffline(a, 1)(true)
	maintenanceTransportOffline(a, 2)(true)
	if w := submitSelected(t, a, []string{"main", "remote"}, true); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	run, _ := a.loadMaintenance()
	if strings.Join(run.Selected, ",") != "main,remote" || strings.Join(run.Skipped, ",") != "remote" || len(run.Hosts) != 1 {
		t.Fatalf("wrong skipped scope: %+v", run)
	}
	if w := submitSelected(t, a, []string{"main"}, true); w.Code != 409 {
		t.Fatal("running selection changed")
	}
	run = tickMaintenance(t, a, 3)
	if run.Status != "success" || agents[1].posts != 0 {
		t.Fatalf("skip scope failed: %+v", run)
	}
}
