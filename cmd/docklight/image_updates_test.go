package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"docklight/internal/imageupdates"
	"docklight/internal/management"
	"github.com/google/uuid"
)

func imageUpdateApp(t *testing.T) (*App, *atomic.Int32) {
	t.Helper()
	a := newTestApp(t)
	calls := &atomic.Int32{}
	maintenance := &maintenanceAgent{info: management.MaintenanceInfo{Version: 1, HostID: "host-nas", State: management.MaintenanceState{NodeID: uuid.NewString()}}}
	old := "sha256:" + strings.Repeat("a", 64)
	containers := []management.Container{{ID: "one", Name: "web-1", Image: "registry.example/web:latest", ImageID: old, Project: "web", Service: "app", State: "running"}, {ID: "two", Name: "web-2", Image: "registry.example/web:latest", ImageID: old, Project: "web", Service: "app", State: "running"}}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != "GET" && !(r.Method == "POST" && r.URL.Path == "/v1/maintenance")) || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
			t.Error("unauthenticated/mutating background request")
		}
		switch r.URL.Path {
		case "/v1/image-updates":
			calls.Add(1)
			snapshot := management.ImageSnapshot{Version: 1, NodeID: "node-one", Hostname: "nas-main"}
			for _, c := range containers {
				snapshot.Containers = append(snapshot.Containers, management.ImageContainer{ID: c.ID, Name: c.Name, Image: c.Image, ImageID: c.ImageID, Project: c.Project, Service: c.Service, OS: "linux", Architecture: "amd64"})
			}
			_ = json.NewEncoder(w).Encode(snapshot)
		case "/v1/inventory":
			_ = json.NewEncoder(w).Encode(management.Inventory{Containers: containers, Resources: []management.Resource{{ID: old, Name: "registry.example/web:latest", Kind: "image", UsedBy: []string{"web-1", "web-2"}}}})
		case "/v1/jobs/update-view":
			_ = json.NewEncoder(w).Encode(management.Job{ID: "update-view", Status: "success", Action: management.Action{Kind: "container", Operation: "upgrade"}})
		default:
			maintenance.serve(w, r)
		}
	}))
	t.Cleanup(agent.Close)
	a.cfg.Hosts = []Host{{ID: "nas", Name: "Main NAS", URL: agent.URL, Token: strings.Repeat("s", 32), AllowHTTP: true, Master: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	a.cfg.Updates = imageupdates.Config{Enabled: true, Interval: time.Hour}
	if v := a.checkMaster(context.Background(), false); v.Error != "" {
		t.Fatal(v.Error)
	}
	if err := a.initImageUpdates(); err != nil {
		t.Fatal(err)
	}
	a.imageMonitor.Resolve = func(context.Context, string, string, string, string) (string, error) {
		return "sha256:" + strings.Repeat("b", 64), nil
	}
	if err := a.imageMonitor.Check(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return a, calls
}

func TestImageUpdateUIUsesCacheAndCountsServices(t *testing.T) {
	a, calls := imageUpdateApp(t)
	r := httptest.NewRequest("GET", "/docklight/", nil)
	d := PageData{HostID: "nas"}
	if err := a.loadInventory(r, &a.cfg.Hosts[0], &d); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("page requested registry/check endpoint")
	}
	if len(d.ContainerGroups) != 1 || d.ContainerGroups[0].Updates != 1 {
		t.Fatalf("replicas counted as services: %+v", d.ContainerGroups)
	}
	w := httptest.NewRecorder()
	a.render(w, r, "inventory", d)
	for _, part := range []string{"↑ 1 service with updates", "↑ Update available", "has-image-update", "Deployed: sha256:", "Registry: sha256:", "image-update-detail"} {
		if !strings.Contains(w.Body.String(), part) {
			t.Fatalf("missing UI %q: %s", part, w.Body.String())
		}
	}
	if strings.Count(w.Body.String(), "↑ Update available") != 3 {
		t.Fatal("container/image update indicators missing")
	}
	// Current inventory must match both container ID and deployed image before using cached status.
	d.Inventory.Containers[0].ImageID = "sha256:" + strings.Repeat("c", 64)
	a.populateImageUpdates(&a.cfg.Hosts[0], &d)
	if d.ImageUpdates["one"].Status != "unknown" || d.ImageUpdates["two"].Status != "available" {
		t.Fatal("stale deployed-image comparison reused")
	}
	// Restart preserves cached status and delivery state without another registry request.
	if err := a.initImageUpdates(); err != nil {
		t.Fatal(err)
	}
	if a.imageMonitor.Host("nas", a.cfg.Hosts[0].URL).Results[0].Status != "available" {
		t.Fatal("restart lost cache")
	}
}

func TestStandaloneUpdateCountDoesNotMergeServiceLabels(t *testing.T) {
	d := PageData{ContainerGroups: groupContainers([]management.Container{{ID: "one", Service: "shared"}, {ID: "two", Service: "shared"}}), ImageUpdates: map[string]imageupdates.Result{"one": {Status: "available"}, "two": {Status: "available"}}}
	(&App{}).groupImageUpdates(&d)
	if d.ContainerGroups[0].Updates != 2 {
		t.Fatal("standalone containers merged by stray Compose service labels")
	}
}

func TestImageUpdateUIStaleErrorAndEscaping(t *testing.T) {
	a, _ := imageUpdateApp(t)
	m := a.imageMonitor
	state := imageupdates.State{Version: 1, Sent: map[string]imageupdates.Delivery{}, Hosts: map[string]imageupdates.HostState{"nas": {Address: a.cfg.Hosts[0].URL, Checked: time.Now().Add(-3 * time.Hour), Results: []imageupdates.Result{{ImageContainer: management.ImageContainer{ID: "one", Image: "<script>bad</script>", ImageID: "old"}, Status: "available", Available: "new"}}}}}
	if err := m.Restore(state); err != nil {
		t.Fatal(err)
	}
	d := PageData{Inventory: management.Inventory{Containers: []management.Container{{ID: "one", Image: "<script>bad</script>", ImageID: "old"}}}}
	a.populateImageUpdates(&a.cfg.Hosts[0], &d)
	if d.ImageUpdates["one"].Status != "stale" || !strings.Contains(d.UpdatesSummary, "stale") {
		t.Fatal("stale result shown current/available")
	}
	// Render through the inventory to check user-controlled image text is escaped.
	d.ContainerGroups = groupContainers(d.Inventory.Containers)
	w := httptest.NewRecorder()
	a.render(w, httptest.NewRequest("GET", "/", nil), "inventory", d)
	if strings.Contains(w.Body.String(), "<script>bad</script>") || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatal("unescaped registry/container metadata")
	}
	state.Hosts["nas"] = imageupdates.HostState{Address: a.cfg.Hosts[0].URL, Error: "Agent unavailable"}
	_ = m.Restore(state)
	a.populateImageUpdates(&a.cfg.Hosts[0], &d)
	if d.ImageUpdates["one"].Status != "unknown" || d.UpdatesSummary != "Agent unavailable" {
		t.Fatal("offline agent reported current")
	}
}

func TestImageUpdateOwnershipAndStateValidation(t *testing.T) {
	a := newTestApp(t)
	a.cfg.Updates = imageupdates.Config{Enabled: true, Interval: time.Hour}
	if a.initImageUpdates() == nil {
		t.Fatal("non-master started watcher")
	}
	if a.imageUpdatesAllowed() {
		t.Fatal("unverified master allowed notifications")
	}
	a.verifiedAt = time.Now()
	if a.imageUpdatesAllowed() {
		t.Fatal("timestamp without successful verification allowed work")
	}
	a, _, _ = maintenanceApp(t)
	a.cfg.Updates = imageupdates.Config{Enabled: true, Interval: time.Hour}
	if v := a.checkMaster(context.Background(), false); v.Error != "" {
		t.Fatal(v.Error)
	}
	if !a.imageUpdatesAllowed() {
		t.Fatal("verified master blocked")
	}
	a.verification.Conflict = true
	if a.imageUpdatesAllowed() {
		t.Fatal("conflicting master allowed notifications")
	}
	a.verification.Conflict = false
	if err := a.maintenanceValue("conflict", "another master"); err != nil {
		t.Fatal(err)
	}
	if a.imageUpdatesAllowed() {
		t.Fatal("durable master conflict ignored")
	}
	_, _ = a.db.Exec(`DELETE FROM maintenance WHERE key='conflict'`)
	a.cfg.Hosts = []Host{{ID: "nas", Name: "NAS", URL: "https://nas.example", Master: true}}
	_, _ = a.db.Exec(`INSERT INTO image_updates(id,value) VALUES(1,'bad json')`)
	if a.initImageUpdates() == nil {
		t.Fatal("corrupt notification state ignored")
	}
}

func TestBrowserImageUpdates(t *testing.T) {
	if os.Getenv("DOCKLIGHT_UI_BROWSER_TEST") != "1" {
		t.Skip("set DOCKLIGHT_UI_BROWSER_TEST=1 with Playwright installed")
	}
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a, _ := imageUpdateApp(t)
			a.cfg.BaseURL = base
			server := httptest.NewServer(a.routes())
			defer server.Close()
			output, err := exec.Command("node", "../../scripts/image-updates-smoke.cjs", server.URL+a.withBase("")).CombinedOutput()
			t.Log(string(output))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImageUpdateWorkerStartupAndCancellation(t *testing.T) {
	a, calls := imageUpdateApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delivered := make(chan struct{}, 1)
	a.imageMonitor.Email = func(context.Context, imageupdates.Event) error { delivered <- struct{}{}; cancel(); return nil }
	done := make(chan struct{})
	go func() { defer close(done); a.imageUpdateLoop(ctx) }()
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("verified master did not run startup detection")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker ignored cancellation")
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected number of scheduled agent snapshots")
	}
}

func TestImageUpdateOwnershipVerificationFailures(t *testing.T) {
	for _, failure := range []string{"offline", "invalid-identity", "registration"} {
		t.Run(failure, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			if v := a.checkMaster(context.Background(), false); v.Error != "" {
				t.Fatal(v.Error)
			}
			if !a.imageUpdatesAllowed() {
				t.Fatal("successful registration did not permit work")
			}
			master := agents[0]
			master.mu.Lock()
			if failure == "offline" {
				master.offline = true
			}
			if failure == "invalid-identity" {
				master.info.State.NodeID = "invalid"
			}
			master.mu.Unlock()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failure == "registration" && r.Method == "POST" {
					http.Error(w, "registration failed", 503)
					return
				}
				master.serve(w, r)
			}))
			defer server.Close()
			a.cfg.Hosts[0].URL = server.URL
			if err := configureHosts(a.cfg.Hosts); err != nil {
				t.Fatal(err)
			}
			v := a.checkMaster(context.Background(), false)
			if v.Error == "" || v.Conflict {
				t.Fatalf("expected non-conflict verification failure: %+v", v)
			}
			if a.verifiedAt.IsZero() || a.imageUpdatesAllowed() {
				t.Fatal("failed verification refreshed permission to work")
			}
		})
	}
}

func TestImageUpdatesRequireFreshFullFleetOwnership(t *testing.T) {
	a, _, _ := maintenanceApp(t)
	if v := a.checkMasterSelected(context.Background(), false, nil, []string{"main"}); v.Error != "" {
		t.Fatal(v.Error)
	}
	if a.imageUpdatesAllowed() {
		t.Fatal("unregistered host permitted fleet-wide checks")
	}
	if v := a.checkMaster(context.Background(), false); v.Error != "" {
		t.Fatal(v.Error)
	}
	if !a.imageUpdatesAllowed() {
		t.Fatal("full ownership verification blocked")
	}
	// A subset check may retain permission only when every other host already belongs to this coordinator.
	if v := a.checkMasterSelected(context.Background(), false, nil, []string{"main"}); v.Error != "" {
		t.Fatal(v.Error)
	}
	if !a.imageUpdatesAllowed() {
		t.Fatal("existing fleet ownership lost during subset verification")
	}
	for _, checked := range []time.Time{time.Now().Add(-3 * time.Minute), time.Now().Add(time.Minute)} {
		a.verifiedAt = checked
		if a.imageUpdatesAllowed() {
			t.Fatal("invalid verification timestamp permitted checks")
		}
	}
}

func TestLoadConfigImageUpdateEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, enabled, interval, errorText string
		master                             bool
		wantInterval                       time.Duration
	}{
		{name: "defaults", wantInterval: 6 * time.Hour},
		{name: "enabled", enabled: "true", interval: "2h", master: true, wantInterval: 2 * time.Hour},
		{name: "trimmed", enabled: " true ", interval: " 3h ", master: true, wantInterval: 3 * time.Hour},
		{name: "enabled-default-interval", enabled: "true", master: true, wantInterval: 6 * time.Hour},
		{name: "no-master", enabled: "true", errorText: "require one master host"},
		{name: "bad-boolean", enabled: "invalid", errorText: "IMAGE_UPDATES_ENABLED"},
		{name: "bad-interval", enabled: "true", interval: "invalid", master: true, errorText: "IMAGE_UPDATES_INTERVAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("ADMIN_PASSWORD", "password")
			t.Setenv("SESSION_SECRET", strings.Repeat("s", 32))
			t.Setenv("HOSTS_FILE", filepath.Join(root, "hosts.yml"))
			t.Setenv("SMTP_FILE", filepath.Join(root, "smtp.yml"))
			t.Setenv("MQTT_FILE", filepath.Join(root, "mqtt.yml"))
			t.Setenv("IMAGE_UPDATES_ENABLED", tc.enabled)
			t.Setenv("IMAGE_UPDATES_INTERVAL", tc.interval)
			text := fmt.Sprintf("hosts:\n  - id: main\n    name: Main\n    url: https://agent.example\n    token: %s\n    master: %t\n", strings.Repeat("t", 32), tc.master)
			if err := os.WriteFile(os.Getenv("HOSTS_FILE"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadConfig()
			if tc.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("invalid update environment accepted: %v", err)
				}
				return
			}
			if err != nil || c.Updates.Enabled != (strings.TrimSpace(tc.enabled) == "true") || c.Updates.Interval != tc.wantInterval {
				t.Fatalf("wrong update environment: enabled=%v interval=%v error=%v", c.Updates.Enabled, c.Updates.Interval, err)
			}
		})
	}
}
