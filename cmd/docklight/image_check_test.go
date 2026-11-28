package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"docklight/internal/imageupdates"
	"docklight/internal/management"
)

func submitImageCheck(t *testing.T, a *App, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	a.db.Exec(`DELETE FROM sessions WHERE id_hash=?`, a.sessionIDHash("test-session"))
	a.routes().ServeHTTP(w, formRequest(a, session(t, a), "/image-updates/check", values))
	return w
}

func TestImageCheckSelectionAndSafety(t *testing.T) {
	for _, scenario := range []string{"empty", "unknown", "duplicate", "wrong-master", "disabled", "unverified", "conflict", "maintenance", "csrf", "unauthenticated"} {
		t.Run(scenario, func(t *testing.T) {
			a, calls := imageUpdateApp(t)
			values := url.Values{"host": {"nas"}, "selected": {"nas"}}
			want := 400
			switch scenario {
			case "empty":
				values.Del("selected")
			case "unknown":
				values.Set("selected", "unknown")
			case "duplicate":
				values.Add("selected", "nas")
			case "wrong-master":
				values.Set("host", "other")
			case "disabled":
				a.imageMonitor = nil
				want = 409
			case "unverified":
				a.verifiedAt = time.Now().Add(-3 * time.Minute)
				want = 409
			case "conflict":
				a.maintenanceValue("conflict", "remembered conflict")
				want = 409
			case "maintenance":
				a.maintenanceValue("run", maintenanceRun{Status: "running"})
				want = 409
			case "csrf":
				values.Set("csrf_token", "wrong")
				want = 403
			case "unauthenticated":
				w := httptest.NewRecorder()
				a.routes().ServeHTTP(w, httptest.NewRequest("POST", "/docklight/image-updates/check", nil))
				if w.Code != 302 {
					t.Fatal("unauthenticated request accepted", w.Code)
				}
				return
			}
			var w *httptest.ResponseRecorder
			if scenario == "csrf" {
				cookie := session(t, a)
				r := httptest.NewRequest("POST", "/docklight/image-updates/check", strings.NewReader(values.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.AddCookie(cookie)
				w = httptest.NewRecorder()
				a.routes().ServeHTTP(w, r)
			} else {
				w = submitImageCheck(t, a, values)
			}
			if w.Code != want || a.imageCheck.Status != "" || calls.Load() != 1 {
				t.Fatalf("unsafe check: status=%d body=%s run=%+v calls=%d", w.Code, w.Body.String(), a.imageCheck, calls.Load())
			}
		})
	}
}

func TestImageCheckQueuesRefreshAndPreservesDelivery(t *testing.T) {
	a, calls := imageUpdateApp(t)
	emails, messages := 0, 0
	a.imageMonitor.Email = func(context.Context, []imageupdates.Event) error { emails++; return nil }
	a.imageMonitor.MQTT = func(context.Context, imageupdates.Event) error { messages++; return nil }
	if err := a.imageMonitor.Deliver(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"host": {"nas"}, "selected": {"nas"}}
	for _, want := range []int{303, 409} {
		w := submitImageCheck(t, a, values)
		if w.Code != want {
			t.Fatalf("queue: %d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 || a.imageCheck.Status != "queued" {
		t.Fatal("request ran scan synchronously")
	}
	if !a.runRequestedImageCheck(context.Background()) || a.imageCheck.Status != "complete" || calls.Load() != 2 {
		t.Fatal("queued scan did not complete")
	}
	if err := a.imageMonitor.Deliver(context.Background(), time.Now().UTC()); err != nil || emails != 1 || messages != 1 {
		t.Fatal("manual check repeated acknowledged notification", err)
	}
	if len(a.imageCheck.Hosts) != 1 || a.imageCheck.Hosts[0].Available != 2 {
		t.Fatalf("missing per-host results: %+v", a.imageCheck)
	}
	// A completed deployment must refresh badges without waiting for the interval.
	a.imageMonitor.Resolve = func(context.Context, string, string, string, string) (string, error) {
		return "sha256:" + strings.Repeat("a", 64), nil
	}
	if w := submitImageCheck(t, a, values); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	a.runRequestedImageCheck(context.Background())
	d := PageData{HostID: "nas"}
	if err := a.loadInventory(httptest.NewRequest("GET", "/", nil), &a.cfg.Hosts[0], &d); err != nil {
		t.Fatal(err)
	}
	if d.ImageUpdates["one"].Status != "current" || d.ContainerGroups[0].Updates != 0 || a.imageCheck.Hosts[0].Current != 2 {
		t.Fatal("fresh check did not clear badges")
	}
	if run, _ := a.loadMaintenance(); run != nil {
		t.Fatal("image check created destructive maintenance run")
	}
}

func TestImageCheckWorkerWakeAndStatus(t *testing.T) {
	a, calls := imageUpdateApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.imageUpdateLoop(ctx) }()
	defer func() { cancel(); <-done }()
	values := url.Values{"host": {"nas"}, "selected": {"nas"}}
	if w := submitImageCheck(t, a, values); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		d := PageData{HostID: "nas"}
		a.populateMaintenance(httptest.NewRequest("GET", "/", nil), &d)
		if d.ImageCheck.Status == "complete" {
			w := httptest.NewRecorder()
			a.render(w, httptest.NewRequest("GET", "/", nil), "maintenance", d)
			for _, text := range []string{"Check image updates now", `formaction="/docklight/image-updates/check"`, "formnovalidate", "2 updates available", "data-image-check-status"} {
				if !strings.Contains(w.Body.String(), text) {
					t.Fatal("missing status/button", text)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not wake for manual check", calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestImageCheckSubsetAndRuntimeFailures(t *testing.T) {
	for _, scenario := range []string{"offline", "unknown", "cancelled", "ownership-lost", "save-failed"} {
		t.Run(scenario, func(t *testing.T) {
			a, _ := imageUpdateApp(t)
			other := a.cfg.Hosts[0]
			other.ID, other.Name, other.Master = "remote", "Remote", false
			a.cfg.Hosts = append(a.cfg.Hosts, other)
			if v := a.checkMaster(context.Background(), false); v.Error != "" {
				t.Fatal(v.Error)
			}
			if err := a.initImageUpdates(); err != nil {
				t.Fatal(err)
			}
			a.imageMonitor.Sources[0].Fetch = func(context.Context) (management.ImageSnapshot, error) {
				t.Fatal("manual subset contacted unselected source")
				return management.ImageSnapshot{}, nil
			}
			fetches := 0
			a.imageMonitor.Sources[1].Fetch = func(context.Context) (management.ImageSnapshot, error) {
				fetches++
				if scenario == "offline" {
					return management.ImageSnapshot{}, errors.New("private upstream error")
				}
				return management.ImageSnapshot{Version: management.ImageSnapshotVersion, NodeID: "remote", Containers: []management.ImageContainer{{ID: "remote-one", Error: "unavailable"}}}, nil
			}
			if w := submitImageCheck(t, a, url.Values{"host": {"nas"}, "selected": {"remote"}}); w.Code != 303 {
				t.Fatal(w.Body.String())
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "cancelled":
				cancel()
			case "ownership-lost":
				a.verification.OwnershipVerified = false
			case "save-failed":
				a.imageMonitor.Save = func(imageupdates.State) error { return errors.New("private database error") }
			}
			a.runRequestedImageCheck(ctx)
			run := a.imageCheck
			if len(run.Hosts) != 1 || run.Hosts[0].Name != "Remote" {
				t.Fatal("wrong result scope", run)
			}
			if scenario == "cancelled" || scenario == "ownership-lost" || scenario == "save-failed" {
				if run.Status != "incomplete" {
					t.Fatal("failure reported complete", run)
				}
			} else if run.Status != "complete" || fetches != 1 {
				t.Fatal("subset not completed", run, fetches)
			}
			if scenario == "offline" && run.Hosts[0].Detail == "" || scenario == "unknown" && run.Hosts[0].Unknown != 1 {
				t.Fatal("unavailable metadata hidden", run)
			}
			w := httptest.NewRecorder()
			d := PageData{HostID: "nas"}
			a.populateMaintenance(httptest.NewRequest("GET", "/", nil), &d)
			a.render(w, httptest.NewRequest("GET", "/", nil), "maintenance", d)
			if strings.Contains(w.Body.String(), "private upstream") || strings.Contains(w.Body.String(), "private database") {
				t.Fatal("private errors leaked")
			}
		})
	}
}

func TestBrowserManualImageCheck(t *testing.T) {
	if os.Getenv("DOCKLIGHT_UI_BROWSER_TEST") != "1" {
		t.Skip("requires Playwright")
	}
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a, _ := imageUpdateApp(t)
			a.cfg.BaseURL = base
			a.imageMonitor.Resolve = func(ctx context.Context, _, _, _, _ string) (string, error) {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(2 * time.Second):
					return "sha256:" + strings.Repeat("a", 64), nil
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); a.imageUpdateLoop(ctx) }()
			defer func() { cancel(); <-done }()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Keep ownership fresh without touching external services.
				a.verificationMu.Lock()
				a.verifiedAt = time.Now()
				a.verificationMu.Unlock()
				a.routes().ServeHTTP(w, r)
			}))
			defer server.Close()
			output, err := exec.Command("node", "../../scripts/manual-image-check-smoke.cjs", server.URL+a.withBase("")).CombinedOutput()
			t.Log(string(output))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
