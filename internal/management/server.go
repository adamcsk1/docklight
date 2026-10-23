package management

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"docklight/internal/buildinfo"

	"github.com/google/uuid"
)

type storedJob struct {
	Job
	Helper           string
	Coordinator, Run string
}

type Server struct {
	Engine      *Engine
	token       [32]byte
	mu          sync.Mutex
	refreshMu   sync.Mutex
	jobs        map[string]*storedJob
	busy        bool
	maintenance MaintenanceState
}

func NewServer(engine *Engine, token string) (*Server, error) {
	if len(token) < 32 {
		return nil, errors.New("AGENT_TOKEN must contain at least 32 characters")
	}
	if engine.Run == nil {
		engine.Run = Command
	}
	if engine.Timeout == 0 {
		engine.Timeout = 10 * time.Minute
	}
	if engine.BuildTimeout == 0 {
		engine.BuildTimeout = 2 * time.Hour
	}
	if !filepath.IsAbs(engine.Root) || !filepath.IsAbs(engine.DataDir) {
		return nil, errors.New("STACKS_ROOT and DATA_DIR must be absolute")
	}
	if err := os.MkdirAll(engine.DataDir, 0700); err != nil {
		return nil, err
	}
	s := &Server{Engine: engine, token: sha256.Sum256([]byte(token)), jobs: map[string]*storedJob{}}
	if err := s.initMaintenance(); err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(engine.DataDir, "job-*.json"))
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var j storedJob
		if err = json.Unmarshal(b, &j); err != nil {
			return nil, fmt.Errorf("invalid job file %s: %w", file, err)
		}
		if _, err = uuid.Parse(j.ID); err != nil {
			return nil, errors.New("invalid stored job ID")
		}
		if j.Status == "running" {
			if j.Helper != "" {
				j.Status = "updating"
			} else {
				j.Status = "failed"
				j.Output += "\nAgent restarted; outcome unknown. Inspect host before retrying."
			}
		}
		s.jobs[j.ID] = &j
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/inventory", s.inventory)
	mux.HandleFunc("POST /v1/jobs", s.submit)
	mux.HandleFunc("GET /v1/jobs/{id}", s.job)
	mux.HandleFunc("GET /v1/maintenance", s.maintenanceInfo)
	mux.HandleFunc("POST /v1/maintenance", s.maintenanceAction)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		header := r.Header.Get("Authorization")
		given := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
		if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare(given[:], s.token[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) inventory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	inv, err := s.Engine.Inventory(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	inv.Commit = buildinfo.Commit
	writeJSON(w, inv)
}

func (s *Server) save(j *storedJob) error {
	return s.saveState("job-"+j.ID+".json", j)
}

func (s *Server) saveState(name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Engine.DataDir, ".job-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.Engine.DataDir, name)); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		// Commit the directory entry before allowing a destructive operation.
		dir, err := os.Open(s.Engine.DataDir)
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}

func (s *Server) refresh(id string) {
	s.mu.Lock()
	needsRefresh := s.jobs[id] != nil && s.jobs[id].Status == "updating"
	s.mu.Unlock()
	if !needsRefresh {
		return
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	j := s.jobs[id]
	if j == nil || j.Status != "updating" {
		s.mu.Unlock()
		return
	}
	candidate := *j
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, output, err := s.Engine.helperResult(ctx, candidate.Helper)
	if err != nil {
		return
	} // A transient Docker failure must not permit concurrent mutations.
	candidate.Status, candidate.Output = status, output
	s.mu.Lock()
	if s.jobs[id] != j || j.Status != "updating" {
		s.mu.Unlock()
		return
	}
	if err := s.save(&candidate); err != nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if status == "success" || status == "failed" {
		// Persist the result before releasing its Docker logs and old image references.
		_ = s.Engine.removeFinishedHelper(ctx, candidate.Helper, candidate.Action.ID)
	}
	s.mu.Lock()
	*j = candidate
	s.mu.Unlock()
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var a Action
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		http.Error(w, "invalid action", 400)
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		http.Error(w, "trailing action data", 400)
		return
	}
	if err := ValidateAction(a); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	owner, run, jobID := r.Header.Get("X-Docklight-Coordinator"), r.Header.Get("X-Docklight-Run"), r.Header.Get("X-Docklight-Job")
	if err := s.authorizeMaintenance(owner, run); err != nil {
		s.mu.Unlock()
		http.Error(w, err.Error(), 409)
		return
	}
	if jobID != "" {
		if !validUUID(jobID) || run == "" {
			s.mu.Unlock()
			http.Error(w, "invalid maintenance job identity", 400)
			return
		}
		if old := s.jobs[jobID]; old != nil {
			if old.Action != a || old.Coordinator != owner || old.Run != run {
				s.mu.Unlock()
				http.Error(w, "job identity conflict", 409)
				return
			}
			result := old.Job
			s.mu.Unlock()
			writeJSON(w, result)
			return
		}
	} else if run != "" {
		s.mu.Unlock()
		http.Error(w, "maintenance job identity required", 400)
		return
	}
	var updating []string
	for id, j := range s.jobs {
		if j.Status == "updating" {
			updating = append(updating, id)
		}
	}
	s.mu.Unlock()
	for _, id := range updating {
		s.refresh(id)
	}
	s.mu.Lock()
	if err := s.authorizeMaintenance(owner, run); err != nil {
		s.mu.Unlock()
		http.Error(w, err.Error(), 409)
		return
	}
	if jobID != "" {
		if old := s.jobs[jobID]; old != nil {
			if old.Action != a || old.Coordinator != owner || old.Run != run {
				s.mu.Unlock()
				http.Error(w, "job identity conflict", 409)
				return
			}
			result := old.Job
			s.mu.Unlock()
			writeJSON(w, result)
			return
		}
	}
	if s.busy {
		s.mu.Unlock()
		http.Error(w, "another host operation is running (builds can take up to two hours)", 409)
		return
	}
	for _, j := range s.jobs {
		if j.Status == "updating" {
			s.mu.Unlock()
			http.Error(w, "management update is still running", 409)
			return
		}
	}
	j := &storedJob{Job: Job{ID: uuid.NewString(), Action: a, Status: "running", Started: time.Now().UTC().Format(time.RFC3339)}}
	if jobID != "" {
		j.ID, j.Coordinator, j.Run = jobID, owner, run
	}
	if a.Operation == "update" {
		j.Helper = HelperName(a)
	}
	if err := s.save(j); err != nil {
		s.mu.Unlock()
		http.Error(w, "cannot persist job", 500)
		return
	}
	s.jobs[j.ID] = j
	s.busy = true
	result := j.Job
	s.mu.Unlock()
	go s.execute(j.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, result)
}

type jobWriter struct {
	s  *Server
	id string
}

func (w jobWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	j := w.s.jobs[w.id]
	j.Output += string(p)
	if len(j.Output) > maxOutput {
		j.Output = "[earlier output truncated]\n" + j.Output[len(j.Output)-maxOutput:]
	}
	return len(p), nil
}

func (s *Server) execute(id string) {
	s.mu.Lock()
	a := s.jobs[id].Action
	s.mu.Unlock()
	timeout := s.Engine.Timeout
	if a.Operation == "build" {
		timeout = s.Engine.BuildTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out := jobWriter{s, id}
	var err error
	if a.Operation == "update" {
		err = s.Engine.Update(ctx, a, out)
	} else {
		err = s.Engine.Execute(ctx, a, out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	j.Status = "success"
	if err != nil {
		j.Status = "failed"
		j.Output += "\n" + err.Error()
		var uncertain helperLaunchError
		if errors.As(err, &uncertain) {
			j.Status = "updating"
			j.Output += "\nHelper launch outcome uncertain; further mutations blocked until verified."
		}
	} else if a.Operation == "update" {
		j.Status = "updating"
	}
	if saveErr := s.save(j); saveErr != nil {
		if j.Status != "updating" {
			j.Status = "failed"
		}
		j.Output += "\nCannot persist final result; inspect host before retrying."
	}
	s.busy = false
	s.prune()
}

func (s *Server) prune() {
	var done []*storedJob
	for _, j := range s.jobs {
		if j.Status == "success" || j.Status == "failed" {
			done = append(done, j)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Started > done[j].Started })
	for _, j := range done[min(100, len(done)):] {
		if j.Run != "" && j.Run == s.maintenance.Run {
			continue
		}
		if err := os.Remove(filepath.Join(s.Engine.DataDir, "job-"+j.ID+".json")); err == nil {
			delete(s.jobs, j.ID)
		}
	}
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.refresh(id)
	s.mu.Lock()
	j := s.jobs[id]
	if j == nil {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	result := j.Job
	s.mu.Unlock()
	writeJSON(w, result)
}
