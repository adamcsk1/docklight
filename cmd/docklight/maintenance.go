package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"docklight/internal/management"
	"github.com/google/uuid"
)

type maintenanceHost struct {
	ID, HostID, NodeID string
	Locked             bool
}
type maintenanceStep struct {
	Host, Service, Role, Target, Job, Status, Before, After, Detail string
	BeforeBuild, AfterBuild                                         string
	Final                                                           bool
	Started                                                         int64
}
type maintenanceRun struct {
	ID, Status, Message, Config string
	Stalled                     int64
	Claim                       management.MasterClaim
	SkipOffline                 bool
	Skipped                     []string
	Hosts                       []maintenanceHost
	Steps                       []maintenanceStep
}
type masterView struct {
	Claim       management.MasterClaim
	Infos       map[string]management.MaintenanceInfo
	Skipped     []string
	Error       string
	Conflict    bool
	OfflineOnly bool
}

type coordinatorContextKey struct{}

func (app *App) populateMaintenance(r *http.Request, d *PageData) {
	master := app.masterHost()
	if master == nil {
		return
	}
	d.Master = d.HostID == master.ID
	if app.maintenanceMu.TryLock() {
		ctx, cancel := context.WithTimeout(r.Context(), maintenanceVerificationTimeout+5*time.Second)
		v := app.checkMaster(ctx, false)
		cancel()
		app.maintenanceMu.Unlock()
		d.MaintenanceError, d.Blocked, d.MaintenanceCanSkip = v.Error, v.Conflict, v.OfflineOnly
	}
	var conflict string
	if err := app.db.QueryRow(`SELECT value FROM maintenance WHERE key='conflict'`).Scan(&conflict); err == nil {
		d.Blocked = true
		d.MaintenanceCanSkip = false
		_ = json.Unmarshal([]byte(conflict), &d.MaintenanceError)
	}
	run, err := app.loadMaintenance()
	if err != nil {
		d.Blocked = true
		d.MaintenanceCanSkip = false
		d.MaintenanceError = "Cannot read durable maintenance state"
		return
	}
	if run != nil && run.Status != "success" && !run.SkipOffline {
		d.MaintenanceCanSkip = false
	}
	d.Maintenance = run
}

func (app *App) allowManagement(w http.ResponseWriter, r *http.Request) bool {
	if app.masterHost() == nil {
		return true
	}
	if !app.maintenanceMu.TryLock() {
		if r.Method == http.MethodPost {
			http.Redirect(w, r, app.withBase("/")+"?host="+url.QueryEscape(r.PostForm.Get("host"))+"&notice=maintenance-busy", http.StatusSeeOther)
		} else {
			w.Header().Set("Retry-After", "3")
			http.Error(w, "maintenance verification in progress; retry shortly", http.StatusServiceUnavailable)
		}
		return false
	}
	defer app.maintenanceMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v := app.checkMaster(ctx, false)
	if v.Conflict {
		http.Error(w, "Master conflict: management blocked. Reconnect for details.", 409)
		return false
	}
	run, err := app.loadMaintenance()
	if err != nil || run != nil && run.Status == "running" {
		http.Error(w, "maintenance active or state unavailable", 409)
		return false
	}
	return true
}

func (app *App) maintenanceStatus(w http.ResponseWriter, r *http.Request) {
	master := app.masterHost()
	if master == nil || r.URL.Query().Get("host") != master.ID {
		http.Error(w, "master host required", 400)
		return
	}
	d := PageData{HostID: master.ID}
	app.populateMaintenance(r, &d)
	if d.Blocked {
		http.Error(w, "master conflict", 409)
		return
	}
	app.render(w, r, "maintenance", d)
}

func (app *App) masterHost() *Host {
	for i := range app.cfg.Hosts {
		if app.cfg.Hosts[i].Master {
			return &app.cfg.Hosts[i]
		}
	}
	return nil
}

func (app *App) maintenanceValue(key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = app.db.Exec(`INSERT INTO maintenance(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(b))
	return err
}
func (app *App) loadMaintenance() (*maintenanceRun, error) {
	var text string
	err := app.db.QueryRow(`SELECT value FROM maintenance WHERE key='run'`).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var run maintenanceRun
	if err := json.Unmarshal([]byte(text), &run); err != nil {
		return nil, err
	}
	return &run, nil
}

type maintenanceHTTPError struct{ Status int }

func (e maintenanceHTTPError) Error() string {
	return fmt.Sprintf("agent maintenance request rejected (HTTP %d)", e.Status)
}

var (
	errMaintenanceUnavailable = errors.New("agent offline or unavailable")
	errMaintenanceTLS         = errors.New("agent TLS verification failed")
)

func (h *Host) maintenanceCall(ctx context.Context, method, path string, body, result any, headers map[string]string) error {
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, h.URL+path, &b)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := h.client.Do(req)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("maintenance connection failed: host=%q error=%v", h.ID, err)
		}
		var certificateError *tls.CertificateVerificationError
		var recordError tls.RecordHeaderError
		var alertError tls.AlertError
		if errors.As(err, &certificateError) || errors.As(err, &recordError) || errors.As(err, &alertError) {
			return errMaintenanceTLS
		}
		return errMaintenanceUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return maintenanceHTTPError{res.StatusCode}
	}
	if result == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(result)
}

func (h *Host) maintenanceCommand(ctx context.Context, op string, claim management.MasterClaim, run string) error {
	return h.maintenanceCall(ctx, "POST", "/v1/maintenance", management.MaintenanceRequest{Operation: op, Claim: claim, Run: run, Confirmed: op == "unregister"}, nil, nil)
}

// Caller holds maintenanceMu. A remembered conflict clears only after a full verified check.
func (app *App) checkMaster(ctx context.Context, discovery bool) masterView {
	return app.checkMasterSkipping(ctx, discovery, nil)
}

func maintenanceVerificationError(err error) string {
	if errors.Is(err, errMaintenanceUnavailable) || errors.Is(err, errMaintenanceTLS) {
		return err.Error()
	}
	var rejection maintenanceHTTPError
	if errors.As(err, &rejection) {
		switch rejection.Status {
		case 401, 403:
			return "maintenance authentication failed; check the host token"
		case 400, 404, 405, 501:
			return "maintenance verification unavailable (update older agents individually)"
		default:
			return fmt.Sprintf("maintenance verification failed (HTTP %d)", rejection.Status)
		}
	}
	return "invalid maintenance response"
}

const maintenanceVerificationTimeout = 30 * time.Second

func (app *App) checkMasterSkipping(ctx context.Context, discovery bool, skipOffline map[string]bool) masterView {
	v := masterView{Infos: map[string]management.MaintenanceInfo{}}
	master := app.masterHost()
	if master == nil {
		return v
	}
	type result struct {
		info management.MaintenanceInfo
		err  error
	}
	results := make([]result, len(app.cfg.Hosts))
	path := "/v1/maintenance"
	if discovery {
		path += "?discovery=true"
	}
	// Independent requests keep one hanging host from consuming the fleet's deadline.
	var wg sync.WaitGroup
	for i := range app.cfg.Hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hostCtx, cancel := context.WithTimeout(ctx, maintenanceVerificationTimeout)
			defer cancel()
			h := &app.cfg.Hosts[i]
			results[i].err = h.maintenanceCall(hostCtx, "GET", path, nil, &results[i].info, nil)
		}()
	}
	wg.Wait()
	var problems []string
	offlineProblems := 0
	for i := range app.cfg.Hosts {
		h := &app.cfg.Hosts[i]
		info, err := results[i].info, results[i].err
		if err != nil {
			if errors.Is(err, errMaintenanceUnavailable) {
				if h.ID != master.ID && skipOffline[h.ID] {
					v.Skipped = append(v.Skipped, h.ID)
					continue
				}
				offlineProblems++
			}
			problems = append(problems, h.ID+": "+maintenanceVerificationError(err))
			continue
		}
		_, nodeErr := uuid.Parse(info.State.NodeID)
		if info.Version != 1 || info.HostID == "" || nodeErr != nil {
			problems = append(problems, h.ID+": invalid maintenance identity")
			continue
		}
		v.Infos[h.ID] = info
		if info.State.Owner.Coordinator != "" && info.State.Owner.Coordinator != app.coordinator {
			v.Conflict = true
			problems = append(problems, fmt.Sprintf("Master conflict on %s: coordinator %s, master %s (%s)", h.ID, info.State.Owner.Coordinator, info.State.Owner.Master, info.State.Owner.Name))
		}
	}
	discoveryProblems := len(problems)
	_, masterAvailable := v.Infos[master.ID]
	offlineOnly := masterAvailable && !v.Conflict && offlineProblems > 0 && offlineProblems == discoveryProblems
	if info, ok := v.Infos[master.ID]; ok {
		v.Claim = management.MasterClaim{Coordinator: app.coordinator, Master: info.HostID, Name: master.Name}
		for id, info := range v.Infos {
			if info.State.Owner.Coordinator == app.coordinator && info.State.Owner.Master != v.Claim.Master {
				v.Conflict = true
				problems = append(problems, "Master conflict on "+id+": registered master differs from hosts.yml")
			}
		}
	}
	if v.Conflict {
		if err := app.maintenanceValue("conflict", strings.Join(problems, "; ")); err != nil {
			problems = append(problems, "cannot persist conflict state")
		}
	} else if len(problems) == 0 {
		var acquired []*Host
		var ordered []*Host
		for i := range app.cfg.Hosts {
			if _, ok := v.Infos[app.cfg.Hosts[i].ID]; ok {
				ordered = append(ordered, &app.cfg.Hosts[i])
			}
		}
		sort.Slice(ordered, func(i, j int) bool { return v.Infos[ordered[i].ID].HostID < v.Infos[ordered[j].ID].HostID })
		for _, h := range ordered {
			if err := h.maintenanceCommand(ctx, "register", v.Claim, ""); err != nil {
				problems = append(problems, h.ID+": registration failed; reconnect to verify ownership")
				var rejection maintenanceHTTPError
				if errors.As(err, &rejection) && rejection.Status == 409 {
					v.Conflict = true
				}
				for _, old := range acquired {
					if err := old.maintenanceCommand(ctx, "unregister", v.Claim, ""); err != nil {
						problems = append(problems, old.ID+": registration cleanup requires reconciliation")
					}
				}
				break
			}
			if v.Infos[h.ID].State.Owner.Coordinator == "" {
				acquired = append(acquired, h)
			}
		}
		if len(problems) == 0 && len(v.Skipped) == 0 {
			if _, err := app.db.Exec(`DELETE FROM maintenance WHERE key='conflict'`); err != nil {
				problems = append(problems, "cannot clear durable conflict state")
				v.Conflict = true
			} else {
				app.masterConflict = ""
			}
		}
		if v.Conflict {
			_ = app.maintenanceValue("conflict", strings.Join(problems, "; "))
		}
	}
	var remembered string
	if err := app.db.QueryRow(`SELECT value FROM maintenance WHERE key='conflict'`).Scan(&remembered); err == nil {
		var message string
		if !v.Conflict && json.Unmarshal([]byte(remembered), &message) == nil {
			problems = append(problems, message)
		}
		v.Conflict = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		v.Conflict = true
		problems = append(problems, "cannot verify durable conflict state")
	}
	if v.Conflict {
		app.masterConflict = strings.Join(problems, "; ")
	} else if app.masterConflict != "" {
		v.Conflict = true
		problems = append(problems, app.masterConflict)
	}
	if ctx.Err() != nil {
		problems = append(problems, "maintenance verification timed out")
	}
	v.OfflineOnly = offlineOnly && !v.Conflict && len(problems) == discoveryProblems
	v.Error = strings.Join(problems, "; ")
	return v
}

func (app *App) configurationDigest() string {
	type host struct {
		ID, URL, Token, CA string
		Master             bool
	}
	var hosts []host
	for _, h := range app.cfg.Hosts {
		hosts = append(hosts, host{h.ID, h.URL, h.Token, h.CAFile, h.Master})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	b, _ := json.Marshal(hosts)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (app *App) preflight(v masterView, previous *maintenanceRun) (*maintenanceRun, error) {
	if v.Error != "" {
		return nil, errors.New(v.Error)
	}
	run := &maintenanceRun{ID: uuid.NewString(), Status: "running", Config: app.configurationDigest(), Claim: v.Claim}
	if previous != nil {
		if previous.Config != run.Config || previous.Claim.Coordinator != run.Claim.Coordinator || previous.Claim.Master != run.Claim.Master {
			return nil, errors.New("configuration or master identity changed; restore original configuration before recovery")
		}
		run.ID = previous.ID
		run.Claim = previous.Claim
		run.SkipOffline = previous.SkipOffline
		run.Skipped = append([]string(nil), previous.Skipped...)
	} else {
		run.SkipOffline = len(v.Skipped) > 0
		run.Skipped = append([]string(nil), v.Skipped...)
	}
	self := os.Getenv("UI_CONTAINER_ID")
	if self == "" {
		self, _ = os.Hostname()
	}
	master := app.masterHost()
	if master == nil {
		return nil, errors.New("master host required")
	}
	var blockers []error
	seenHosts, seenNodes := map[string]bool{}, map[string]bool{}
	finalServices := 0
	hosts := append([]Host(nil), app.cfg.Hosts...)
	sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].Master && !hosts[j].Master })
	skipped := map[string]bool{}
	for _, id := range run.Skipped {
		skipped[id] = true
	}
	for _, h := range hosts {
		if skipped[h.ID] {
			continue
		}
		info, ok := v.Infos[h.ID]
		if !ok {
			blockers = append(blockers, errors.New(h.ID+": maintenance verification unavailable"))
			continue
		}
		if seenHosts[info.HostID] {
			if !seenNodes[info.State.NodeID] {
				blockers = append(blockers, errors.New("multiple agents manage the same Docker host; configure one agent per host"))
			}
			continue
		}
		if seenNodes[info.State.NodeID] {
			blockers = append(blockers, errors.New("duplicate agent node identity on different Docker hosts"))
			continue
		}
		seenHosts[info.HostID], seenNodes[info.State.NodeID] = true, true
		if previous != nil {
			found := false
			for _, old := range previous.Hosts {
				if old.ID == h.ID && old.HostID == info.HostID && old.NodeID == info.State.NodeID {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("%s: host identity changed; inspect before recovery", h.ID)
			}
		}
		if info.Busy {
			blockers = append(blockers, fmt.Errorf("%s: another operation is active; retry after reconciliation", h.ID))
		}
		run.Hosts = append(run.Hosts, maintenanceHost{ID: h.ID, HostID: info.HostID, NodeID: info.State.NodeID})
		services := map[string]int{}
		agents, selfAgents := 0, 0
		for _, t := range info.Targets {
			if t.ID == "" || len(t.ID) > 256 || t.Image == "" || t.Role != "agent" && t.Role != "ui" {
				blockers = append(blockers, fmt.Errorf("%s: invalid management target metadata", h.ID))
				continue
			}
			if t.Error != "" || t.Service == "" || t.State != "running" || (t.Health != "" && t.Health != "healthy") {
				blockers = append(blockers, fmt.Errorf("%s/%s: update target unavailable or unhealthy: %s", h.ID, t.Name, t.Error))
			}
			if t.Role == "agent" {
				agents++
				if t.Self {
					selfAgents++
				} else {
					blockers = append(blockers, fmt.Errorf("%s: another agent container is not managed by this endpoint", h.ID))
				}
			}
			final := t.Role == "ui" && len(self) >= 12 && strings.HasPrefix(t.ID, self)
			if final && h.ID != master.ID {
				blockers = append(blockers, errors.New("coordinating UI must run on the configured master host"))
			}
			if index, ok := services[t.Service]; ok {
				if run.Steps[index].Role != t.Role {
					blockers = append(blockers, errors.New("mixed UI/agent roles in one Compose service"))
				}
				if run.Steps[index].Before != t.Image {
					blockers = append(blockers, errors.New("service replicas use different images; resolve before maintenance"))
				}
				if final && !run.Steps[index].Final {
					run.Steps[index].Final = true
					finalServices++
				}
				continue
			}
			services[t.Service] = len(run.Steps)
			if final {
				finalServices++
			}
			commit := t.Commit
			if t.Role == "agent" {
				commit = info.Commit
			}
			if commit == "" {
				commit = "unknown"
			}
			run.Steps = append(run.Steps, maintenanceStep{Host: h.ID, Service: t.Service, Role: t.Role, Target: t.ID, Status: "pending", Before: t.Image, BeforeBuild: commit, Final: final})
		}
		if agents != 1 || selfAgents != 1 {
			blockers = append(blockers, fmt.Errorf("%s: exactly one identifiable agent required", h.ID))
		}
	}
	if finalServices != 1 {
		blockers = append(blockers, errors.New("cannot uniquely identify coordinating UI; use default Docker hostname or UI_CONTAINER_ID"))
	}
	if len(blockers) > 0 {
		return nil, errors.Join(blockers...)
	}
	sort.Slice(run.Hosts, func(i, j int) bool { return run.Hosts[i].HostID < run.Hosts[j].HostID })
	sort.SliceStable(run.Steps, func(i, j int) bool {
		rank := func(s maintenanceStep) int {
			if s.Final {
				return 3
			}
			if s.Role == "ui" {
				return 2
			}
			if s.Host == master.ID {
				return 1
			}
			return 0
		}
		a, b := rank(run.Steps[i]), rank(run.Steps[j])
		if a != b {
			return a < b
		}
		return run.Steps[i].Host+run.Steps[i].Service < run.Steps[j].Host+run.Steps[j].Service
	})
	if previous != nil {
		var retained []maintenanceStep
		for i := range run.Steps {
			for _, old := range previous.Steps {
				if old.Host == run.Steps[i].Host && old.Service == run.Steps[i].Service {
					current := run.Steps[i]
					if old.Role != current.Role || old.Final != current.Final {
						return nil, errors.New("management service role or coordinating UI changed; inspect before recovery")
					}
					if old.Status == "success" || old.Job != "" && old.Status != "failed" {
						run.Steps[i] = old
						if old.Status == "success" {
							run.Steps[i].After = current.Before
							run.Steps[i].AfterBuild = current.BeforeBuild
						}
					}
					retained = append(retained, run.Steps[i])
					break
				}
			}
		}
		run.Steps = retained
		for _, old := range previous.Steps {
			found := false
			for _, step := range run.Steps {
				if old.Host == step.Host && old.Service == step.Service {
					found = true
				}
			}
			if !found {
				return nil, errors.New("previous target disappeared; inspect before recovery")
			}
		}
	}
	return run, nil
}

func maintenanceSkipHosts(run *maintenanceRun) map[string]bool {
	skipped := map[string]bool{}
	if run != nil && run.SkipOffline {
		for _, id := range run.Skipped {
			skipped[id] = true
		}
	}
	return skipped
}

func (app *App) maintenanceStart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	master := app.masterHost()
	if master == nil || r.PostForm.Get("host") != master.ID || r.PostForm.Get("confirmed") != "yes" {
		http.Error(w, "master host and explicit confirmation required", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	app.maintenanceMu.Lock()
	defer app.maintenanceMu.Unlock()
	if ctx.Err() != nil {
		http.Error(w, "verification timed out; no new maintenance run submitted", 409)
		return
	}
	old, err := app.loadMaintenance()
	if err != nil {
		http.Error(w, "cannot load maintenance state", 500)
		return
	}
	if old != nil && old.Status == "running" {
		http.Error(w, "maintenance already running", 409)
		return
	}
	var previous *maintenanceRun
	if old != nil && old.Status != "success" {
		previous = old
	}
	skipOffline := r.PostForm.Get("skip_offline") == "yes"
	skippable := map[string]bool{}
	if previous != nil {
		if previous.SkipOffline != skipOffline {
			http.Error(w, "offline device selection cannot change during maintenance recovery; keep the original Skip offline devices setting", 409)
			return
		}
		skippable = maintenanceSkipHosts(previous)
	} else if skipOffline {
		for _, h := range app.cfg.Hosts {
			if h.ID != master.ID {
				skippable[h.ID] = true
			}
		}
	}
	v := app.checkMasterSkipping(ctx, true, skippable)
	run, err := app.preflight(v, previous)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	run.SkipOffline = skipOffline
	if err = app.maintenanceValue("run", run); err != nil {
		http.Error(w, "cannot persist maintenance run", 500)
		return
	}
	http.Redirect(w, r, app.withBase("/")+"?host="+master.ID, http.StatusSeeOther)
}

func (app *App) maintenanceLoop(ctx context.Context) {
	for {
		app.maintenanceTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (app *App) saveRun(run *maintenanceRun) bool { return app.maintenanceValue("run", run) == nil }

func (run *maintenanceRun) stall(message string) {
	run.Message = message
	if run.Stalled == 0 {
		run.Stalled = time.Now().Unix()
	}
	if time.Now().Unix()-run.Stalled > 15*60 {
		run.Status = "attention"
	}
}

func (app *App) maintenanceTick(parent context.Context) {
	app.maintenanceMu.Lock()
	defer app.maintenanceMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	run, err := app.loadMaintenance()
	if err != nil {
		return
	}
	if run == nil || run.Status != "running" {
		_ = app.checkMaster(ctx, false)
		return
	}
	if run.Config != app.configurationDigest() {
		run.Status = "attention"
		run.Message = "Configuration changed; restore original hosts.yml before recovery"
		app.saveRun(run)
		return
	}
	// Renew every locked host before discovery or polling can consume its lease.
	for _, rh := range run.Hosts {
		if rh.Locked {
			h, _ := app.host(rh.ID)
			if err := h.maintenanceCommand(ctx, "renew", run.Claim, run.ID); err != nil {
				run.stall(rh.ID + ": ownership renewal unavailable; reconnecting")
				app.saveRun(run)
				return
			}
		}
	}
	v := app.checkMasterSkipping(ctx, true, maintenanceSkipHosts(run))
	if v.Error != "" {
		run.stall(v.Error)
		if v.Conflict {
			run.Status = "attention"
		}
		app.saveRun(run)
		return
	}
	run.Stalled = 0
	for i := range run.Hosts {
		rh := &run.Hosts[i]
		info := v.Infos[rh.ID]
		if info.HostID != rh.HostID || info.State.NodeID != rh.NodeID {
			run.Status = "attention"
			run.Message = rh.ID + ": host identity changed"
			app.saveRun(run)
			return
		}
		if !rh.Locked {
			h, _ := app.host(rh.ID)
			if err := h.maintenanceCommand(ctx, "acquire", run.Claim, run.ID); err != nil {
				run.Message = rh.ID + ": cannot acquire host; no further updates submitted"
				app.releaseRun(ctx, run)
				run.Status = "attention"
				app.saveRun(run)
				return
			}
			rh.Locked = true
			if !app.saveRun(run) {
				return
			}
		}
	}
	for i := range run.Steps {
		step := &run.Steps[i]
		if step.Status == "success" || step.Status == "failed" {
			continue
		}
		blocked := false
		for _, before := range run.Steps[:i] {
			if before.Status != "success" && (step.Role == "ui" && before.Role == "agent" || step.Final) {
				blocked = true
			}
		}
		if blocked {
			continue
		}
		h, _ := app.host(step.Host)
		info := v.Infos[step.Host]
		var targets []management.UpdateTarget
		for _, target := range info.Targets {
			if target.Service == step.Service {
				targets = append(targets, target)
			}
		}
		if len(targets) == 0 {
			step.Detail = "Service unavailable; inspect host and reconcile the existing job before retry"
			if step.Job != "" {
				run.Status = "attention"
				run.Message = step.Detail
			} else {
				step.Status = "failed"
			}
			app.saveRun(run)
			return
		}
		if step.Job == "" {
			step.Target = targets[0].ID
			step.Job = uuid.NewString()
			step.Status = "submitting"
			step.Started = time.Now().Unix()
			if !app.saveRun(run) {
				return
			}
		}
		var job management.Job
		err := h.maintenanceCall(ctx, "GET", "/v1/jobs/"+step.Job, nil, &job, nil)
		var rejection maintenanceHTTPError
		if errors.As(err, &rejection) && rejection.Status == 404 {
			if step.Status != "submitting" {
				run.Status = "attention"
				run.Message = "Previously accepted job disappeared; inspect host before recovery"
				app.saveRun(run)
				return
			}
			// Persisted job identity makes replay safe even after a lost acceptance response.
			a := management.Action{Kind: "container", ID: step.Target, Operation: "update", Confirmed: true}
			err = h.maintenanceCall(ctx, "POST", "/v1/jobs", a, &job, map[string]string{"X-Docklight-Coordinator": run.Claim.Coordinator, "X-Docklight-Run": run.ID, "X-Docklight-Job": step.Job})
		}
		if err != nil {
			step.Detail = "Reconnecting; submission outcome may be unknown. No duplicate update submitted."
			if time.Now().Unix()-step.Started > 15*60 {
				run.Status = "attention"
				run.Message = "Update outcome needs inspection before retry"
			}
			app.saveRun(run)
			return
		}
		if job.ID != step.Job || job.Action.ID != step.Target || job.Action.Kind != "container" || job.Action.Operation != "update" {
			run.Status = "attention"
			run.Message = "Unexpected job identity; inspect agent"
			app.saveRun(run)
			return
		}
		step.Detail = job.Output
		if job.Status == "failed" {
			step.Status = "failed"
			app.saveRun(run)
			return
		}
		if job.Status != "success" {
			step.Status = "updating"
			if time.Now().Unix()-step.Started > 15*60 {
				run.Status = "attention"
				run.Message = "Update timed out; reconcile existing helper before retry"
			}
			app.saveRun(run)
			return
		}
		// Read service state after completion, never verify against a pre-job snapshot.
		var fresh management.MaintenanceInfo
		if err := h.maintenanceCall(ctx, "GET", "/v1/maintenance?discovery=true", nil, &fresh, nil); err != nil {
			step.Detail = "Helper complete; waiting to verify service"
			if time.Now().Unix()-step.Started > 15*60 {
				run.Status = "attention"
				run.Message = step.Detail
			}
			app.saveRun(run)
			return
		}
		targets = nil
		for _, target := range fresh.Targets {
			if target.Service == step.Service {
				targets = append(targets, target)
			}
		}
		if fresh.HostID != info.HostID || fresh.State.NodeID != info.State.NodeID || len(targets) == 0 {
			run.Status = "attention"
			run.Message = "Host or service identity changed during verification"
			app.saveRun(run)
			return
		}
		for _, target := range targets {
			if target.Role != step.Role || step.Role == "agent" && !target.Self || target.State != "running" || target.Health != "" && target.Health != "healthy" {
				step.Detail = "Helper completed; waiting for service health"
				if time.Now().Unix()-step.Started > 15*60 {
					run.Status = "attention"
					run.Message = "Updated service did not become healthy"
				}
				app.saveRun(run)
				return
			}
		}
		step.Status = "success"
		step.After = targets[0].Image
		step.AfterBuild = targets[0].Commit
		if step.Role == "agent" {
			step.AfterBuild = fresh.Commit
		}
		if step.AfterBuild == "" {
			step.AfterBuild = "unknown"
		}
		if step.After == step.Before {
			step.Detail += "\nVerified running; image unchanged (already current or pinned)."
		} else {
			step.Detail += "\nVerified running image changed."
		}
		if targets[0].Health == "" {
			step.Detail += " No Docker health check configured; application health is not verified."
		}
		run.Message = "Verified " + step.Host + " " + step.Role
		app.saveRun(run)
		return
	}
	all := true
	for _, step := range run.Steps {
		if step.Status != "success" {
			all = false
		}
	}
	run.Status = "success"
	run.Message = "All agents and UIs verified."
	if len(run.Skipped) > 0 {
		run.Message = "Selected agents and UIs verified. Skipped offline devices: " + strings.Join(run.Skipped, ", ") + "."
	}
	if !all {
		run.Status = "attention"
		run.Message = "Partial maintenance; agents must succeed before UIs, and all other targets before coordinating UI."
	}
	app.releaseRun(ctx, run)
	app.saveRun(run)
}

func (app *App) releaseRun(ctx context.Context, run *maintenanceRun) {
	for i := range run.Hosts {
		rh := &run.Hosts[i]
		if rh.Locked {
			h, err := app.host(rh.ID)
			if err == nil && h.maintenanceCommand(ctx, "release", run.Claim, run.ID) == nil {
				rh.Locked = false
			} else {
				run.Message += " " + rh.ID + ": ownership release needs reconciliation."
				run.Status = "attention"
			}
		}
	}
}
