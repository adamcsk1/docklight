package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ordinaryRequest(s *Server, id, operation string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(`{"kind":"container","id":"`+testID+`","operation":"`+operation+`"}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("X-Docklight-Request", id)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestConcurrentOrdinaryRequestsExecuteOnce(t *testing.T) {
	f := newFixture(t)
	run := f.e.Run
	var executions atomic.Int32
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if len(args) > 1 && args[0] == "container" && args[1] == "stop" {
			executions.Add(1)
		}
		return run(ctx, dir, out, args...)
	}
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 16)
	for range 16 {
		go func() { <-start; responses <- ordinaryRequest(s, id, "stop") }()
	}
	close(start)
	accepted := 0
	for range 16 {
		w := <-responses
		if w.Code == http.StatusAccepted {
			accepted++
		} else if w.Code != http.StatusOK {
			t.Fatalf("replay: %d %s", w.Code, w.Body.String())
		}
		var job Job
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil || job.ID != id {
			t.Fatalf("replay identity: %s (%v)", w.Body.String(), err)
		}
	}
	if job := waitJob(t, s, id); job.Status != "success" || accepted != 1 || executions.Load() != 1 {
		t.Fatalf("job=%+v accepted=%d executions=%d", job, accepted, executions.Load())
	}
}

func TestOrdinaryAcceptanceRacesMaintenanceReservation(t *testing.T) {
	for attempt := range 8 {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			f := newFixture(t)
			run := f.e.Run
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var executions atomic.Int32
			f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
				if len(args) > 1 && args[0] == "container" && args[1] == "stop" {
					executions.Add(1)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return run(ctx, dir, out, args...)
			}
			s, err := NewServer(f.e, testToken)
			if err != nil {
				t.Fatal(err)
			}
			claim := MasterClaim{Coordinator: uuid.NewString(), Master: "main", Name: "Main"}
			if w := maintenanceRequest(t, s, "register", claim, "", false); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			start := make(chan struct{})
			action, reservation := make(chan int, 1), make(chan int, 1)
			id := uuid.NewString()
			go func() { <-start; action <- ordinaryRequest(s, id, "stop").Code }()
			go func() {
				<-start
				reservation <- maintenanceRequest(t, s, "acquire", claim, uuid.NewString(), false).Code
			}()
			close(start)
			actionCode, reservationCode := <-action, <-reservation
			if !(actionCode == 202 && reservationCode == 409 || actionCode == 409 && reservationCode == 200) {
				t.Fatalf("nonexclusive acceptance: action=%d reservation=%d", actionCode, reservationCode)
			}
			once.Do(func() { close(release) })
			if actionCode == 202 {
				if job := waitJob(t, s, id); job.Status != "success" || executions.Load() != 1 {
					t.Fatalf("accepted job=%+v executions=%d", job, executions.Load())
				}
			} else if executions.Load() != 0 {
				t.Fatal("reserved host executed ordinary operation")
			}
		})
	}
}

func TestSubmissionRechecksAfterHelperReconciliation(t *testing.T) {
	for _, scenario := range []string{"replay", "different-action", "reservation", "new-owner"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			run := f.e.Run
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var executions atomic.Int32
			f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
				if args[0] == "inspect" {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					return errors.New("helper inspection unavailable")
				}
				if len(args) > 1 && args[0] == "container" && args[1] == "stop" {
					executions.Add(1)
				}
				return run(ctx, dir, out, args...)
			}
			s, err := NewServer(f.e, testToken)
			if err != nil {
				t.Fatal(err)
			}
			helperID, id := uuid.NewString(), uuid.NewString()
			s.jobs[helperID] = &storedJob{Job: Job{ID: helperID, Status: "updating"}, Helper: "helper"}
			owner := uuid.NewString()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(`{"kind":"container","id":"`+testID+`","operation":"stop"}`))
				r.Header.Set("Authorization", "Bearer "+testToken)
				r.Header.Set("X-Docklight-Request", id)
				r.Header.Set("X-Docklight-Coordinator", owner)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				done <- w
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("submission never released mutex for reconciliation")
			}
			// Another request changes host state while the first request inspects Docker.
			s.mu.Lock()
			s.jobs[helperID].Status = "success"
			s.mu.Unlock()
			want, count := http.StatusConflict, int32(0)
			if scenario == "replay" || scenario == "different-action" {
				action := Action{Kind: "container", ID: testID, Operation: "stop"}
				if scenario == "different-action" {
					action.Operation = "start"
				}
				r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(fmt.Sprintf(`{"kind":"container","id":%q,"operation":%q}`, testID, action.Operation)))
				r.Header.Set("Authorization", "Bearer "+testToken)
				r.Header.Set("X-Docklight-Request", id)
				r.Header.Set("X-Docklight-Coordinator", owner)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusAccepted {
					t.Fatalf("competing acceptance: %d %s", w.Code, w.Body.String())
				}
				if job := waitJob(t, s, id); job.Status != "success" {
					t.Fatal(job)
				}
				if scenario == "replay" {
					want, count = http.StatusOK, 1
				}
			} else {
				claim := MasterClaim{Coordinator: owner, Master: "main", Name: "Main"}
				if scenario == "new-owner" {
					claim.Coordinator = uuid.NewString()
				}
				if w := maintenanceRequest(t, s, "register", claim, "", false); w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				if scenario == "reservation" {
					if w := maintenanceRequest(t, s, "acquire", claim, uuid.NewString(), false); w.Code != 200 {
						t.Fatal(w.Body.String())
					}
				}
			}
			once.Do(func() { close(release) })
			select {
			case w := <-done:
				if w.Code != want || executions.Load() != count {
					t.Fatalf("recheck: %d %s executions=%d", w.Code, w.Body.String(), executions.Load())
				}
				if want == http.StatusOK {
					var job Job
					if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil || job.ID != id {
						t.Fatalf("wrong replay: %s", w.Body.String())
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("submission did not finish")
			}
		})
	}
}

func TestOrdinaryFinalPersistenceFailureDoesNotReplay(t *testing.T) {
	f := newFixture(t)
	data := f.e.DataDir
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	run := f.e.Run
	var executions atomic.Int32
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		err := run(ctx, dir, out, args...)
		if len(args) > 1 && args[0] == "container" && args[1] == "stop" {
			executions.Add(1)
			f.e.DataDir = blocked
		}
		return err
	}
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if w := ordinaryRequest(s, id, "stop"); w.Code != http.StatusAccepted {
		t.Fatal(w.Body.String())
	}
	job := waitJob(t, s, id)
	if job.Status != "failed" || !strings.Contains(job.Output, "Cannot persist final result") {
		t.Fatalf("lost warning: %+v", job)
	}
	if w := ordinaryRequest(s, id, "stop"); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	f.e.DataDir = data
	restarted, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	job = waitJob(t, restarted, id)
	if job.Status != "failed" || !strings.Contains(job.Output, "outcome unknown") {
		t.Fatalf("restart hid uncertainty: %+v", job)
	}
	if w := ordinaryRequest(restarted, id, "stop"); w.Code != http.StatusOK || executions.Load() != 1 {
		t.Fatalf("repeated uncertain operation: %d executions=%d", w.Code, executions.Load())
	}
}

func TestPruneRetainsNewestActiveAndMaintenanceJobs(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.NewString()
	s.maintenance.Run = runID
	ids := make([]string, 105)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range ids {
		ids[i] = uuid.NewString()
		j := &storedJob{Job: Job{ID: ids[i], Status: "success", Started: base.Add(time.Duration(i)).Format(time.RFC3339Nano), Action: Action{Kind: "container", ID: testID, Operation: "stop"}}}
		if i < 2 {
			j.Run = runID
		}
		if err := s.save(j); err != nil {
			t.Fatal(err)
		}
		s.jobs[j.ID] = j
	}
	active := &storedJob{Job: Job{ID: uuid.NewString(), Status: "running", Started: base.Add(-time.Hour).Format(time.RFC3339)}}
	if err := s.save(active); err != nil {
		t.Fatal(err)
	}
	s.jobs[active.ID] = active
	s.prune()
	for i, id := range ids {
		_, err := os.Stat(filepath.Join(f.e.DataDir, "job-"+id+".json"))
		keep := i < 2 || i >= 5
		if (s.jobs[id] != nil) != keep || keep && err != nil || !keep && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retention index=%d keep=%v err=%v", i, keep, err)
		}
	}
	if s.jobs[active.ID] == nil {
		t.Fatal("active job pruned")
	}
	active.Status = "updating"
	s.prune()
	if s.jobs[active.ID] == nil {
		t.Fatal("uncertain helper pruned")
	}
	// Reservation has ended; retained ordinary identities must still replay after restart.
	restarted, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.jobs) != 103 || restarted.jobs[ids[0]] == nil || restarted.jobs[ids[1]] == nil || restarted.jobs[active.ID] == nil {
		t.Fatal("restart lost retained jobs")
	}
	if w := ordinaryRequest(restarted, ids[104], "stop"); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if len(f.calls) != 0 {
		t.Fatal("retained replay executed Docker")
	}
}

func TestPruneRemovalFailureRetainsReplayIdentity(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	old := uuid.NewString()
	for i := range 101 {
		id := uuid.NewString()
		if i == 0 {
			id = old
		}
		j := &storedJob{Job: Job{ID: id, Status: "success", Started: time.Unix(int64(i), 0).UTC().Format(time.RFC3339), Action: Action{Kind: "container", ID: testID, Operation: "stop"}}}
		if err := s.save(j); err != nil {
			t.Fatal(err)
		}
		s.jobs[id] = j
	}
	path := filepath.Join(f.e.DataDir, "job-"+old+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "block-removal"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.prune()
	if s.jobs[old] == nil {
		t.Fatal("forgot identity despite failed removal")
	}
	if w := ordinaryRequest(s, old, "stop"); w.Code != http.StatusOK || len(f.calls) != 0 {
		t.Fatal("failed eviction lost replay protection")
	}
	if err := os.Remove(filepath.Join(path, "block-removal")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.save(s.jobs[old]); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if w := ordinaryRequest(restarted, old, "stop"); w.Code != http.StatusOK || len(f.calls) != 0 {
		t.Fatal("restart lost retained replay identity")
	}
	restarted.prune()
	if restarted.jobs[old] != nil {
		t.Fatal("did not retry eviction")
	}
}

func TestComposeUpgradeSurvivesDisconnectAndReplaysOnce(t *testing.T) {
	f := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	run := f.e.Run
	var pulls atomic.Int32
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if args[0] == "compose" && slices.Contains(args, "pull") {
			pulls.Add(1)
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return run(ctx, dir, out, args...)
	}
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	body := `{"kind":"container","id":"` + testID + `","operation":"upgrade","scope":"stack"}`
	submit := func(ctx context.Context, server *Server, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("X-Docklight-Request", id)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if w := submit(ctx, s, body); w.Code != http.StatusAccepted {
		t.Fatalf("acceptance: %d %s", w.Code, w.Body.String())
	}
	cancel() // Closing the browser/UI connection must not cancel Compose.
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upgrade never reached pull")
	}
	if w := submit(context.Background(), s, body); w.Code != http.StatusOK {
		t.Fatalf("lost-response replay: %d %s", w.Code, w.Body.String())
	}
	if w := submit(context.Background(), s, strings.Replace(body, "upgrade", "recreate", 1)); w.Code != http.StatusConflict {
		t.Fatal("request identity reused for a different action")
	}
	current := agentRequest(s, "GET", "/v1/jobs/current", "", testToken)
	var job Job
	if err := json.Unmarshal(current.Body.Bytes(), &job); err != nil || job.ID != id || job.Status != "running" {
		t.Fatalf("active rediscovery: %s %v", current.Body.String(), err)
	}
	once.Do(func() { close(release) })
	if job := waitJob(t, s, id); job.Status != "success" {
		t.Fatalf("disconnect interrupted upgrade: %+v", job)
	}
	restarted, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	current = agentRequest(restarted, "GET", "/v1/jobs/current", "", testToken)
	if err := json.Unmarshal(current.Body.Bytes(), &job); err != nil || job.ID != id || job.Status != "success" {
		t.Fatalf("completed rediscovery after restart: %s %v", current.Body.String(), err)
	}
	if w := submit(context.Background(), restarted, body); w.Code != http.StatusOK || pulls.Load() != 1 {
		t.Fatalf("completed replay launched another upgrade: %d, pulls=%d", w.Code, pulls.Load())
	}
	if w := agentRequest(restarted, "GET", "/v1/jobs/current", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("current job exposed without authentication")
	}
}

func TestCurrentJobPrefersActiveThenNewest(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if w := agentRequest(s, "GET", "/v1/jobs/current", "", testToken); strings.TrimSpace(w.Body.String()) != "null" {
		t.Fatalf("empty host: %s", w.Body.String())
	}
	older, newer := uuid.NewString(), uuid.NewString()
	s.jobs[older] = &storedJob{Job: Job{ID: older, Status: "running", Started: "2026-01-01T00:00:00Z"}}
	s.jobs[newer] = &storedJob{Job: Job{ID: newer, Status: "success", Started: "2026-01-01T00:00:00.000000001Z"}}
	for _, want := range []string{older, newer} {
		w := agentRequest(s, "GET", "/v1/jobs/current", "", testToken)
		var job Job
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil || job.ID != want {
			t.Fatalf("current job: %s, want %s", w.Body.String(), want)
		}
		s.jobs[older].Status = "success"
	}
	for _, id := range []string{"invalid", "../outside"} {
		r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(`{"kind":"container","id":"x","operation":"stop"}`))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("X-Docklight-Request", id)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid request identity accepted: %d", w.Code)
		}
	}
}
