package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"docklight/internal/management"
	"github.com/google/uuid"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func maintenanceTransportOffline(a *App, host int) func(bool) {
	offline := false
	transport := a.cfg.Hosts[host].client.Transport
	a.cfg.Hosts[host].client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if offline {
			return nil, errors.New("device offline")
		}
		return transport.RoundTrip(r)
	})
	return func(value bool) { offline = value }
}

func submitMaintenance(t *testing.T, a *App, cookie *http.Cookie, skip bool) *httptest.ResponseRecorder {
	t.Helper()
	values := url.Values{"host": {"main"}, "confirmed": {"yes"}}
	if skip {
		values.Set("skip_offline", "yes")
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, cookie, "/maintenance", values))
	return w
}

func TestMaintenanceSkipsOnlyInitiallyOfflineDevices(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	cookie := session(t, a)
	setRemoteOffline := maintenanceTransportOffline(a, 1)
	setRemoteOffline(true)

	r := httptest.NewRequest("GET", "https://docklight.local/docklight/?host=main", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	body := w.Body.String()
	formStart := strings.Index(body, `action="/docklight/maintenance"`)
	if w.Code != 200 || formStart < 0 {
		t.Fatalf("maintenance form missing: %d %s", w.Code, body)
	}
	form := body[formStart:]
	formEnd := strings.Index(form, "</form>")
	skipPosition := strings.Index(form, `name="skip_offline"`)
	confirmPosition := strings.Index(form, `name="confirmed"`)
	if formEnd < 0 || skipPosition < 0 || confirmPosition < 0 || skipPosition > confirmPosition {
		t.Fatalf("offline skip control missing or misplaced: %s", form)
	}
	if strings.Contains(form[:formEnd], `type="submit" disabled`) {
		t.Fatal("skippable offline device disabled maintenance form")
	}

	w = submitMaintenance(t, a, cookie, false)
	requireDocklightError(t, w, http.StatusConflict, "remote: agent offline or unavailable")
	if run, err := a.loadMaintenance(); err != nil || run != nil {
		t.Fatalf("offline device updated without opt-in: %+v, %v", run, err)
	}

	w = submitMaintenance(t, a, cookie, true)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("skip request rejected: %d %s", w.Code, w.Body.String())
	}
	started, err := a.loadMaintenance()
	if err != nil || started == nil || !started.SkipOffline || strings.Join(started.Skipped, ",") != "remote" || len(started.Hosts) != 1 || started.Hosts[0].ID != "main" || len(started.Steps) != 2 {
		t.Fatalf("wrong offline run scope: %+v, %v", started, err)
	}
	for _, step := range started.Steps {
		if step.Host != "main" {
			t.Fatalf("offline device retained as update target: %+v", step)
		}
	}

	// Restart from durable state before any update, then bring the skipped device back.
	restarted := &App{cfg: a.cfg, db: a.db}
	if err := restarted.initDB(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	run := tickMaintenance(t, restarted, 1)
	if run.ID != started.ID || strings.Join(run.Skipped, ",") != "remote" {
		t.Fatalf("restart lost offline scope: %+v", run)
	}
	setRemoteOffline(false)
	run = tickMaintenance(t, restarted, 2)
	if run.Status != "success" || !strings.Contains(run.Message, "Skipped offline devices: remote") || agents[0].posts != 2 || agents[1].posts != 0 {
		t.Fatalf("wrong partial maintenance result: %+v, main posts %d, remote posts %d", run, agents[0].posts, agents[1].posts)
	}
	for _, host := range run.Hosts {
		if host.Locked {
			t.Fatalf("successful partial run retained lock: %+v", host)
		}
	}

	r = httptest.NewRequest("GET", "https://docklight.local/docklight/maintenance?host=main", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	restarted.routes().ServeHTTP(w, r)
	body = w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `data-maintenance-skipped`) || !strings.Contains(body, "skipped — offline") || !strings.Contains(body, "remote · no updates submitted") {
		t.Fatalf("skipped device not marked in UI: %d %s", w.Code, body)
	}
}

func TestMaintenanceHangingOfflineHostDoesNotBlockFleetVerification(t *testing.T) {
	for _, skip := range []bool{false, true} {
		name := "required"
		if skip {
			name = "skip"
		}
		t.Run(name, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			// Put the hanging host first to catch sequential head-of-line blocking.
			a.cfg.Hosts[0], a.cfg.Hosts[1] = a.cfg.Hosts[1], a.cfg.Hosts[0]
			offlineStarted := make(chan struct{})
			releaseOffline := make(chan struct{})
			deadline := make(chan time.Duration, 1)
			var offlineOnce, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseOffline) }) }
			a.cfg.Hosts[0].client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				offlineOnce.Do(func() {
					remaining := time.Duration(0)
					if until, ok := r.Context().Deadline(); ok {
						remaining = time.Until(until)
					}
					deadline <- remaining
					close(offlineStarted)
				})
				<-releaseOffline
				return nil, errors.New("device offline")
			})
			mainVerified := make(chan struct{})
			var mainOnce sync.Once
			mainAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/v1/maintenance" {
					mainOnce.Do(func() { close(mainVerified) })
				}
				agents[0].serve(w, r)
			}))
			defer mainAgent.Close()
			a.cfg.Hosts[1].URL = mainAgent.URL

			values := url.Values{"host": {"main"}, "confirmed": {"yes"}}
			if skip {
				values.Set("skip_offline", "yes")
			}
			r := formRequest(a, session(t, a), "/maintenance", values)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				a.routes().ServeHTTP(w, r)
				close(done)
			}()
			defer func() {
				release()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
				}
			}()
			wait := func(ch <-chan struct{}) bool {
				select {
				case <-ch:
					return true
				case <-time.After(2 * time.Second):
					return false
				}
			}
			if !wait(offlineStarted) {
				t.Fatal("offline verification did not start")
			}
			if remaining := <-deadline; remaining <= 0 || remaining > maintenanceVerificationTimeout {
				t.Errorf("offline request inherited fleet deadline instead of independent timeout: %v", remaining)
			}
			mainReached := wait(mainVerified)
			release()
			if !wait(done) {
				t.Fatal("maintenance request did not finish")
			}
			if !mainReached {
				t.Fatal("hanging offline host prevented master verification")
			}
			run, err := a.loadMaintenance()
			if err != nil {
				t.Fatal(err)
			}
			if skip {
				if w.Code != http.StatusSeeOther || run == nil || strings.Join(run.Skipped, ",") != "remote" || len(run.Hosts) != 1 || run.Hosts[0].ID != "main" {
					t.Fatalf("hanging offline host blocked skip: %d %s %+v", w.Code, w.Body.String(), run)
				}
			} else {
				requireDocklightError(t, w, http.StatusConflict, "remote: agent offline or unavailable")
				if strings.Contains(w.Body.String(), "main: agent offline") || run != nil {
					t.Fatalf("offline host made healthy master unavailable: %s %+v", w.Body.String(), run)
				}
			}
		})
	}
}

func TestMaintenanceHostDeadlineKeepsDashboardSkipAvailable(t *testing.T) {
	a, _, _ := maintenanceApp(t)
	a.cfg.Hosts[0], a.cfg.Hosts[1] = a.cfg.Hosts[1], a.cfg.Hosts[0]
	a.cfg.Hosts[0].client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	r := httptest.NewRequest("GET", "/docklight/?host=main", nil)
	d := PageData{HostID: "main"}
	a.populateMaintenance(r, &d)
	if !d.MaintenanceCanSkip || d.Blocked || d.MaintenanceError != "remote: agent offline or unavailable" {
		t.Fatalf("host timeout disabled offline skipping or poisoned healthy hosts: %+v", d)
	}
	w := httptest.NewRecorder()
	a.render(w, r, "maintenance", d)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="skip_offline"`) || strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("skip form unavailable after host timeout: %d %s", w.Code, w.Body.String())
	}
}

func TestMaintenanceParentCancellationStillBlocksSkipping(t *testing.T) {
	a, _, _ := maintenanceApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.cfg.Hosts[1].client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	defer cancel()
	v := a.checkMasterSkipping(ctx, true, map[string]bool{"remote": true})
	if !strings.Contains(v.Error, "maintenance verification timed out") || v.OfflineOnly {
		t.Fatalf("parent cancellation treated as skippable host failure: %+v", v)
	}
	if run, err := a.preflight(v, nil); err == nil || run != nil {
		t.Fatalf("canceled verification accepted a run: %+v %v", run, err)
	}
}

func TestMaintenanceSkipRejectsUnsafeFailures(t *testing.T) {
	for _, scenario := range []string{"master-offline", "authentication", "old-agent", "agent-error", "docker-discovery", "conflict", "remembered-conflict", "tls", "tls-alert"} {
		t.Run(scenario, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			var reject *httptest.Server
			switch scenario {
			case "master-offline":
				maintenanceTransportOffline(a, 0)(true)
			case "authentication", "agent-error":
				status := http.StatusUnauthorized
				if scenario == "agent-error" {
					status = http.StatusInternalServerError
				}
				reject = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "private diagnostic", status) }))
				defer reject.Close()
				a.cfg.Hosts[1].URL = reject.URL
			case "old-agent":
				agents[1].info.Version = 0
			case "docker-discovery":
				agents[1].offline = true
			case "conflict":
				agents[1].info.State.Owner = management.MasterClaim{Coordinator: uuid.NewString(), Master: "other", Name: "Other"}
			case "remembered-conflict":
				maintenanceTransportOffline(a, 1)(true)
				if err := a.maintenanceValue("conflict", "remembered master conflict"); err != nil {
					t.Fatal(err)
				}
			case "tls":
				secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
				defer secure.Close()
				a.cfg.Hosts[1].URL = secure.URL
			case "tls-alert":
				a.cfg.Hosts[1].client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, tls.AlertError(40)
				})
			}
			w := submitMaintenance(t, a, session(t, a), true)
			requireDocklightError(t, w, http.StatusConflict, "")
			if strings.Contains(w.Body.String(), "private diagnostic") {
				t.Fatal("agent diagnostic leaked into error page")
			}
			if run, err := a.loadMaintenance(); err != nil || run != nil {
				t.Fatalf("unsafe failure was skipped: %+v, %v", run, err)
			}
			if scenario == "remembered-conflict" {
				var remembered string
				if err := a.db.QueryRow(`SELECT value FROM maintenance WHERE key='conflict'`).Scan(&remembered); err != nil {
					t.Fatalf("offline skip cleared durable conflict: %v", err)
				}
			}
			for _, agent := range agents {
				if agent.posts != 0 {
					t.Fatal("unsafe preflight submitted an update")
				}
			}
		})
	}
}

func TestMaintenanceRecoveryKeepsOfflineSelection(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	cookie := session(t, a)
	setRemoteOffline := maintenanceTransportOffline(a, 1)
	setRemoteOffline(true)
	if w := submitMaintenance(t, a, cookie, true); w.Code != http.StatusSeeOther {
		t.Fatal(w.Body.String())
	}
	original, err := a.loadMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	agents[0].fail = true
	run := tickMaintenance(t, a, 2)
	if run.Status != "attention" {
		t.Fatalf("failed selected device did not require recovery: %+v", run)
	}
	agents[0].fail = false
	setRemoteOffline(false)

	w := submitMaintenance(t, a, cookie, false)
	requireDocklightError(t, w, http.StatusConflict, "offline device selection cannot change")
	w = submitMaintenance(t, a, cookie, true)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("valid recovery rejected: %d %s", w.Code, w.Body.String())
	}
	run = tickMaintenance(t, a, 3)
	if run.ID != original.ID || run.Status != "success" || strings.Join(run.Skipped, ",") != "remote" || len(run.Steps) != 2 || agents[0].posts != 3 || agents[1].posts != 0 {
		t.Fatalf("recovery changed target scope: %+v, main posts %d, remote posts %d", run, agents[0].posts, agents[1].posts)
	}
}

func TestMaintenanceSkipDoesNotHideSelectedDeviceOutage(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	cookie := session(t, a)
	maintenanceTransportOffline(a, 1)(true)
	if w := submitMaintenance(t, a, cookie, true); w.Code != http.StatusSeeOther {
		t.Fatal(w.Body.String())
	}
	maintenanceTransportOffline(a, 0)(true)
	run := tickMaintenance(t, a, 1)
	if run.Status != "running" || !strings.Contains(run.Message, "main: agent offline or unavailable") || agents[0].posts != 0 || agents[1].posts != 0 {
		t.Fatalf("selected device outage was silently skipped: %+v", run)
	}
	run.Stalled = time.Now().Add(-time.Hour).Unix()
	if !a.saveRun(run) {
		t.Fatal("cannot save stalled run")
	}
	run = tickMaintenance(t, a, 1)
	if run.Status != "attention" || agents[0].posts != 0 || agents[1].posts != 0 {
		t.Fatalf("persistent selected device outage did not block: %+v", run)
	}
}
