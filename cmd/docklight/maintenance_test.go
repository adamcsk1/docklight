package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"docklight/internal/management"
	"github.com/google/uuid"
)

type maintenanceAgent struct {
	mu                                       sync.Mutex
	info                                     management.MaintenanceInfo
	jobs                                     map[string]management.Job
	offline, fail, loseResponse, failAcquire bool
	failService                              string
	jobStatus                                string
	unchanged                                bool
	posts                                    int
	order                                    *[]string
	orderMu                                  *sync.Mutex
}

func maintenanceApp(t *testing.T) (*App, []*maintenanceAgent, *[]string) {
	t.Helper()
	t.Setenv("UI_CONTAINER_ID", "coordinating-ui-id")
	a := newTestApp(t)
	order := []string{}
	orderMu := &sync.Mutex{}
	var agents []*maintenanceAgent
	for _, id := range []string{"main", "remote"} {
		f := &maintenanceAgent{info: management.MaintenanceInfo{Version: 1, HostID: "host-" + id, State: management.MaintenanceState{NodeID: uuid.NewString()}}, jobs: map[string]management.Job{}, order: &order, orderMu: orderMu}
		f.info.Targets = []management.UpdateTarget{{ID: id + "-agent-id", Service: id + "-agent", Role: "agent", Name: "agent", State: "running", Image: "old", Self: true}, {ID: id + "-ui-id", Service: id + "-ui", Role: "ui", Name: "ui", State: "running", Image: "old"}}
		if id == "main" {
			f.info.Targets[1].ID = "coordinating-ui-id-original"
		}
		agent := httptest.NewServer(http.HandlerFunc(f.serve))
		t.Cleanup(agent.Close)
		a.cfg.Hosts = append(a.cfg.Hosts, Host{ID: id, Name: id, URL: agent.URL, Token: strings.Repeat(id, 32), AllowHTTP: true, Master: id == "main"})
		agents = append(agents, f)
	}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	return a, agents, &order
}

func (f *maintenanceAgent) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offline {
		http.Error(w, "offline", 502)
		return
	}
	write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	switch {
	case r.URL.Path == "/v1/maintenance" && r.Method == "GET":
		write(f.info)
	case r.URL.Path == "/v1/inventory":
		write(management.Inventory{})
	case r.URL.Path == "/v1/maintenance":
		var req management.MaintenanceRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad", 400)
			return
		}
		if f.info.State.Owner.Coordinator != "" && (f.info.State.Owner.Coordinator != req.Claim.Coordinator || f.info.State.Owner.Master != req.Claim.Master) {
			http.Error(w, "conflict", 409)
			return
		}
		switch req.Operation {
		case "register":
			f.info.State.Owner = req.Claim
		case "acquire":
			if f.failAcquire {
				http.Error(w, "busy", 409)
				return
			}
			f.info.State.Run = req.Run
			f.info.State.Lease = time.Now().Add(time.Minute).Unix()
		case "renew":
			if f.info.State.Run != req.Run {
				http.Error(w, "lost", 409)
				return
			}
			f.info.State.Lease = time.Now().Add(time.Minute).Unix()
		case "release":
			f.info.State.Run = ""
			f.info.State.Lease = 0
		case "unregister":
			f.info.State.Owner = management.MasterClaim{}
		}
		write(f.info.State)
	case r.URL.Path == "/v1/jobs" && r.Method == "POST":
		var action management.Action
		if json.NewDecoder(r.Body).Decode(&action) != nil {
			http.Error(w, "bad", 400)
			return
		}
		id := r.Header.Get("X-Docklight-Job")
		if old, ok := f.jobs[id]; ok {
			write(old)
			return
		}
		if id == "" || r.Header.Get("X-Docklight-Run") != f.info.State.Run || r.Header.Get("X-Docklight-Coordinator") != f.info.State.Owner.Coordinator {
			http.Error(w, "missing ownership", 409)
			return
		}
		f.posts++
		status := "success"
		if f.fail {
			status = "failed"
		}
		for _, target := range f.info.Targets {
			if target.ID == action.ID && target.Service == f.failService {
				status = "failed"
			}
		}
		if f.jobStatus != "" {
			status = f.jobStatus
		}
		job := management.Job{ID: id, Action: action, Status: status, Output: "helper output <script>not executable</script>"}
		f.jobs[id] = job
		for i := range f.info.Targets {
			target := &f.info.Targets[i]
			if target.ID == action.ID {
				f.orderMu.Lock()
				*f.order = append(*f.order, target.Service)
				f.orderMu.Unlock()
				if status == "success" && !f.unchanged {
					target.ID += "-new"
					target.Image = "new"
				}
			}
		}
		if f.loseResponse {
			f.loseResponse = false
			http.Error(w, "lost response", 502)
			return
		}
		w.WriteHeader(202)
		write(job)
	case strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
		job, ok := f.jobs[strings.TrimPrefix(r.URL.Path, "/v1/jobs/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		write(job)
	default:
		http.NotFound(w, r)
	}
}

func prepareMaintenance(t *testing.T, a *App) *maintenanceRun {
	t.Helper()
	a.maintenanceMu.Lock()
	defer a.maintenanceMu.Unlock()
	v := a.checkMaster(context.Background(), true)
	run, err := a.preflight(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !a.saveRun(run) {
		t.Fatal("cannot save run")
	}
	return run
}
func tickMaintenance(t *testing.T, a *App, n int) *maintenanceRun {
	t.Helper()
	for i := 0; i < n; i++ {
		a.maintenanceTick(context.Background())
	}
	run, err := a.loadMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestMaintenanceOrderingLostResponseAndRestart(t *testing.T) {
	a, agents, order := maintenanceApp(t)
	prepareMaintenance(t, a)
	agents[1].loseResponse = true
	run := tickMaintenance(t, a, 1)
	if run.Steps[0].Status != "submitting" || agents[1].posts != 1 {
		t.Fatalf("lost acceptance not retained: %+v", run)
	}
	// New process uses only durable state; no in-memory workflow survives.
	restarted := &App{cfg: a.cfg, db: a.db}
	if err := restarted.initDB(); err != nil {
		t.Fatal(err)
	}
	if restarted.coordinator != a.coordinator {
		t.Fatal("coordinator identity changed")
	}
	run = tickMaintenance(t, restarted, 8)
	if run.Status != "success" {
		t.Fatalf("%+v", run)
	}
	want := []string{"remote-agent", "main-agent", "remote-ui", "main-ui"}
	if strings.Join(*order, ",") != strings.Join(want, ",") {
		t.Fatalf("wrong order: %v", *order)
	}
	if agents[0].posts != 2 || agents[1].posts != 2 {
		t.Fatal("lost response caused duplicate update")
	}
	for _, step := range run.Steps {
		if step.Status != "success" || step.After != "new" || !strings.Contains(step.Detail, "application health is not verified") {
			t.Fatalf("unverified target %+v", step)
		}
	}
	for _, h := range run.Hosts {
		if h.Locked {
			t.Fatal("successful run retained locks")
		}
	}
}

func TestMaintenancePartialFailureAndRetry(t *testing.T) {
	a, agents, order := maintenanceApp(t)
	agents[1].fail = true
	prepareMaintenance(t, a)
	run := tickMaintenance(t, a, 6)
	if run.Status != "attention" || strings.Join(*order, ",") != "remote-agent,main-agent" {
		t.Fatalf("UI updated after failed agent: %+v %v", run, *order)
	}
	agents[1].fail = false
	v := a.checkMaster(context.Background(), true)
	retry, err := a.preflight(v, run)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != run.ID || retry.Steps[1].Status != "success" || retry.Steps[0].Job != "" {
		t.Fatal("retry repeated successes or reused failed job")
	}
	a.saveRun(retry)
	run = tickMaintenance(t, a, 6)
	if run.Status != "success" || strings.Join(*order, ",") != "remote-agent,main-agent,remote-agent,remote-ui,main-ui" {
		t.Fatalf("bad retry: %+v %v", run, *order)
	}
}

func TestMasterConflictBlocksUIAndPersistsWhileOffline(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	agents[1].info.State.Owner = management.MasterClaim{Coordinator: uuid.NewString(), Master: "elsewhere", Name: "Other <script>"}
	cookie := session(t, a)
	for _, offline := range []bool{false, true} {
		agents[1].offline = offline
		r := httptest.NewRequest("GET", "https://docklight.local/docklight/?host=main", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "management blocked") || strings.Contains(w.Body.String(), "data-inventory ") || strings.Contains(w.Body.String(), "<script>") {
			t.Fatalf("conflict UI: %d %s", w.Code, w.Body.String())
		}
		for _, path := range []string{"/action", "/maintenance"} {
			w = httptest.NewRecorder()
			a.routes().ServeHTTP(w, formRequest(a, cookie, path, url.Values{"host": {"main"}, "confirmed": {"yes"}, "kind": {"container"}, "id": {"x"}, "operation": {"stop"}}))
			if w.Code != 409 {
				t.Fatalf("conflict mutation accepted on %s: %d", path, w.Code)
			}
		}
	}
	agents[1].offline = false
	agents[1].info.State.Owner = management.MasterClaim{}
	if v := a.checkMaster(context.Background(), true); v.Conflict || v.Error != "" {
		t.Fatalf("resolved conflict stuck: %+v", v)
	}
}

func TestMaintenancePreflightBlocksUnsafeTargets(t *testing.T) {
	for _, scenario := range []string{"offline", "busy", "old-agent", "missing-ui", "compose", "unhealthy", "extra-agent", "different-master", "same-host-two-agents", "duplicate-node"} {
		t.Run(scenario, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			switch scenario {
			case "offline":
				agents[1].offline = true
			case "busy":
				agents[1].info.Busy = true
			case "old-agent":
				agents[1].info.Version = 0
			case "missing-ui":
				agents[0].info.Targets = agents[0].info.Targets[:1]
			case "compose":
				agents[1].info.Targets[0].Error = "unavailable"
			case "unhealthy":
				agents[1].info.Targets[1].Health = "unhealthy"
			case "extra-agent":
				agents[1].info.Targets = append(agents[1].info.Targets, management.UpdateTarget{ID: "extra", Role: "agent", Service: "extra", State: "running"})
			case "different-master":
				agents[1].info.State.Owner = management.MasterClaim{Coordinator: a.coordinator, Master: "other", Name: "Other"}
			case "same-host-two-agents":
				agents[1].info.HostID = agents[0].info.HostID
			case "duplicate-node":
				agents[1].info.State.NodeID = agents[0].info.State.NodeID
			}
			v := a.checkMaster(context.Background(), true)
			if run, err := a.preflight(v, nil); err == nil {
				t.Fatalf("unsafe preflight accepted: %+v", run)
			}
			for _, agent := range agents {
				if agent.posts != 0 {
					t.Fatal("preflight mutated a service")
				}
			}
		})
	}
}

func TestMaintenancePartialAcquisitionAndConfigurationChange(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	prepareMaintenance(t, a)
	agents[1].failAcquire = true
	run := tickMaintenance(t, a, 1)
	if run.Status != "attention" || agents[0].posts != 0 || agents[1].posts != 0 || agents[0].info.State.Run != "" {
		t.Fatalf("partial acquisition not rolled back: %+v", run)
	}
	agents[1].failAcquire = false
	v := a.checkMaster(context.Background(), true)
	retry, err := a.preflight(v, run)
	if err != nil {
		t.Fatal(err)
	}
	a.saveRun(retry)
	a.cfg.Hosts[1].URL += "/changed"
	run = tickMaintenance(t, a, 1)
	if run.Status != "attention" || !strings.Contains(run.Message, "Configuration changed") {
		t.Fatalf("configuration silently changed target: %+v", run)
	}
}

func TestMaintenanceFormScopeConfirmationAndAliases(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	cookie := session(t, a)
	for _, values := range []url.Values{{"host": {"main"}}, {"host": {"remote"}, "confirmed": {"yes"}}, {"confirmed": {"yes"}}} {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, formRequest(a, cookie, "/maintenance", values))
		if w.Code != 400 {
			t.Fatalf("bad form accepted: %d", w.Code)
		}
	}
	alias := a.cfg.Hosts[0]
	alias.ID = "alias"
	alias.Master = false
	a.cfg.Hosts = append([]Host{alias}, a.cfg.Hosts...)
	v := a.checkMaster(context.Background(), true)
	run, err := a.preflight(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Hosts) != 2 || len(run.Steps) != 4 {
		t.Fatal("alias duplicated targets")
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, cookie, "/maintenance", url.Values{"host": {"main"}, "confirmed": {"yes"}}))
	if w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, cookie, "/maintenance", url.Values{"host": {"main"}, "confirmed": {"yes"}}))
	if w.Code != 409 {
		t.Fatal("duplicate run accepted")
	}
	for _, agent := range agents {
		if agent.posts != 0 {
			t.Fatal("request ran updates before worker")
		}
	}
}

func TestMaintenanceUnknownJobAndTimeout(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	run := prepareMaintenance(t, a)
	run.Steps[0].Job = uuid.NewString()
	run.Steps[0].Status = "updating"
	run.Steps[0].Started = time.Now().Add(-time.Hour).Unix()
	a.saveRun(run)
	run = tickMaintenance(t, a, 1)
	if run.Status != "attention" || agents[1].posts != 0 || !strings.Contains(run.Message, "disappeared") {
		t.Fatalf("missing accepted job resubmitted: %+v", run)
	}
}

func TestMaintenanceUIFailureBlocksCoordinator(t *testing.T) {
	a, agents, order := maintenanceApp(t)
	agents[1].failService = "remote-ui"
	prepareMaintenance(t, a)
	run := tickMaintenance(t, a, 8)
	if run.Status != "attention" || strings.Join(*order, ",") != "remote-agent,main-agent,remote-ui" {
		t.Fatalf("coordinator replaced despite failed UI: %+v %v", run, *order)
	}
}

func TestMaintenanceWaitsForHealthAndHandlesPinnedImages(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	agents[1].unchanged = true
	prepareMaintenance(t, a)
	run := tickMaintenance(t, a, 1)
	if run.Steps[0].Status != "success" || !strings.Contains(run.Steps[0].Detail, "already current or pinned") {
		t.Fatalf("pinned image misreported: %+v", run)
	}
	// Current target becomes unhealthy after helper success; final UI must never run.
	agents[0].mu.Lock()
	agents[0].info.Targets[0].Health = "starting"
	agents[0].mu.Unlock()
	run = tickMaintenance(t, a, 1)
	if run.Steps[1].Status == "success" {
		t.Fatal("unhealthy agent marked verified")
	}
	run.Steps[1].Started = time.Now().Add(-time.Hour).Unix()
	a.saveRun(run)
	run = tickMaintenance(t, a, 1)
	if run.Status != "attention" {
		t.Fatal("unhealthy service did not time out")
	}
}

func TestMaintenancePendingJobTimeoutAndIdentityMismatch(t *testing.T) {
	for _, scenario := range []string{"timeout", "identity", "missing", "lease", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			agents[1].jobStatus = "updating"
			prepareMaintenance(t, a)
			run := tickMaintenance(t, a, 1)
			step := &run.Steps[0]
			switch scenario {
			case "timeout":
				step.Started = time.Now().Add(-time.Hour).Unix()
			case "identity":
				agents[1].mu.Lock()
				j := agents[1].jobs[step.Job]
				j.Action.ID = "other"
				agents[1].jobs[step.Job] = j
				agents[1].mu.Unlock()
			case "missing":
				agents[1].mu.Lock()
				delete(agents[1].jobs, step.Job)
				agents[1].mu.Unlock()
			case "lease":
				agents[1].mu.Lock()
				agents[1].info.State.Run = "other"
				agents[1].mu.Unlock()
				run.Stalled = time.Now().Add(-time.Hour).Unix()
			case "conflict":
				agents[0].mu.Lock()
				agents[0].info.State.Owner = management.MasterClaim{Coordinator: uuid.NewString(), Master: "other", Name: "Other"}
				agents[0].mu.Unlock()
				run.Stalled = time.Now().Add(-time.Hour).Unix()
			}
			a.saveRun(run)
			run = tickMaintenance(t, a, 1)
			if run.Status != "attention" || agents[0].posts != 0 || agents[1].posts != 1 {
				t.Fatalf("unsafe recovery: %+v", run)
			}
		})
	}
}

func TestMaintenanceStatusScopeAndEscaping(t *testing.T) {
	a, _, _ := maintenanceApp(t)
	cookie := session(t, a)
	for _, host := range []string{"main", "remote"} {
		r := httptest.NewRequest("GET", "https://docklight.local/docklight/maintenance?host="+host, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		want := 200
		if host == "remote" {
			want = 400
		}
		if w.Code != want {
			t.Fatalf("status scope: %d %s", w.Code, w.Body.String())
		}
	}
	run := prepareMaintenance(t, a)
	run.Steps[0].Detail = "<script>unsafe</script>"
	a.saveRun(run)
	r := httptest.NewRequest("GET", "https://docklight.local/docklight/maintenance?host=main", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "<script>") || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatal("unescaped maintenance output")
	}
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, cookie, "/action", url.Values{"host": {"main"}, "kind": {"container"}, "id": {"x"}, "operation": {"stop"}}))
	if w.Code != 409 {
		t.Fatal("normal mutation bypassed active maintenance")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.maintenanceLoop(ctx)
}

func TestMaintenanceReadFailureBlocksMutations(t *testing.T) {
	a, _, _ := maintenanceApp(t)
	cookie := session(t, a)
	if _, err := a.db.Exec(`INSERT INTO maintenance(key,value) VALUES('run','corrupt')`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, formRequest(a, cookie, "/action", url.Values{"host": {"main"}, "kind": {"container"}, "id": {"x"}, "operation": {"stop"}}))
	if w.Code != 409 {
		t.Fatal("corrupt run allowed mutation")
	}
}

func TestMaintenanceReplicaDeduplicationAndBlockerList(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	agents[0].info.Targets = append(agents[0].info.Targets, management.UpdateTarget{ID: "another-ui-replica", Service: "main-ui", Role: "ui", Name: "replica", State: "running", Image: "old"})
	v := a.checkMaster(context.Background(), true)
	run, err := a.preflight(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Steps) != 4 || !run.Steps[3].Final {
		t.Fatal("replica duplicated or coordinator not last")
	}
	agents[0].info.Targets[2].Image = "different"
	agents[1].info.Busy = true
	v = a.checkMaster(context.Background(), true)
	_, err = a.preflight(v, nil)
	if err == nil || !strings.Contains(err.Error(), "different images") || !strings.Contains(err.Error(), "another operation") {
		t.Fatalf("missing preflight blockers: %v", err)
	}
}

func TestMaintenanceNeverSubmitsAfterPersistenceFailure(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	run := prepareMaintenance(t, a)
	// A DB write failure at a submission checkpoint must leave Docker untouched.
	if _, err := a.db.Exec(`CREATE TRIGGER block_maintenance_write BEFORE UPDATE ON maintenance BEGIN SELECT RAISE(FAIL, 'disk unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	a.maintenanceTick(context.Background())
	for _, agent := range agents {
		if agent.posts != 0 {
			t.Fatal("service changed without durable checkpoint")
		}
	}
	if _, err := a.db.Exec(`DROP TRIGGER block_maintenance_write`); err != nil {
		t.Fatal(err)
	}
	if stored, err := a.loadMaintenance(); err != nil || stored.ID != run.ID {
		t.Fatal("lost original run")
	}
}

func TestMaintenanceMissingServiceRetainsUncertainJob(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	agents[1].jobStatus = "updating"
	prepareMaintenance(t, a)
	run := tickMaintenance(t, a, 1)
	jobID := run.Steps[0].Job
	agents[1].mu.Lock()
	target := agents[1].info.Targets[0]
	agents[1].info.Targets = agents[1].info.Targets[1:]
	agents[1].mu.Unlock()
	run = tickMaintenance(t, a, 1)
	if run.Status != "attention" || run.Steps[0].Job != jobID || run.Steps[0].Status == "failed" {
		t.Fatalf("uncertain job lost: %+v", run)
	}
	agents[1].mu.Lock()
	agents[1].info.Targets = append([]management.UpdateTarget{target}, agents[1].info.Targets...)
	j := agents[1].jobs[jobID]
	j.Status = "success"
	agents[1].jobs[jobID] = j
	agents[1].mu.Unlock()
	v := a.checkMaster(context.Background(), true)
	retry, err := a.preflight(v, run)
	if err != nil {
		t.Fatal(err)
	}
	a.saveRun(retry)
	run = tickMaintenance(t, a, 1)
	if run.Steps[0].Status != "success" || agents[1].posts != 1 {
		t.Fatalf("uncertain job was resubmitted: %+v", run)
	}
}

func TestMaintenanceRetryKeepsTargetSnapshot(t *testing.T) {
	a, agents, _ := maintenanceApp(t)
	original := prepareMaintenance(t, a)
	agents[1].mu.Lock()
	agents[1].info.Targets = append(agents[1].info.Targets, management.UpdateTarget{ID: "new-ui-id", Service: "new-ui", Role: "ui", Name: "new UI", State: "running", Image: "old"})
	agents[1].mu.Unlock()
	v := a.checkMaster(context.Background(), true)
	retry, err := a.preflight(v, original)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Steps) != len(original.Steps) {
		t.Fatalf("retry silently expanded targets: %+v", retry.Steps)
	}
}

func TestBrowserMaintenance(t *testing.T) {
	if os.Getenv("DOCKLIGHT_UI_BROWSER_TEST") != "1" {
		t.Skip("requires Playwright")
	}
	for _, base := range []string{"/", "/docklight"} {
		t.Run(base, func(t *testing.T) {
			a, agents, _ := maintenanceApp(t)
			a.cfg.BaseURL = base
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); a.maintenanceLoop(ctx) }()
			defer func() { cancel(); <-done }()
			handler := a.routes()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == a.withBase("/test-master-conflict") {
					agents[1].mu.Lock()
					agents[1].info.State.Owner = management.MasterClaim{Coordinator: uuid.NewString(), Master: "other", Name: "Other"}
					agents[1].mu.Unlock()
					w.WriteHeader(204)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			cmd := exec.Command("node", "../../scripts/maintenance-smoke.cjs", server.URL+a.withBase(""))
			out, err := cmd.CombinedOutput()
			t.Log(string(out))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
