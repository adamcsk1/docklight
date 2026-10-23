package management

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"docklight/internal/buildinfo"
	"github.com/google/uuid"
)

func (s *Server) initMaintenance() error {
	b, err := os.ReadFile(filepath.Join(s.Engine.DataDir, "maintenance.json"))
	if errors.Is(err, os.ErrNotExist) {
		s.maintenance.NodeID = uuid.NewString()
		return s.saveState("maintenance.json", s.maintenance)
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &s.maintenance); err != nil {
		return fmt.Errorf("invalid maintenance state: %w", err)
	}
	if !validUUID(s.maintenance.NodeID) {
		return errors.New("invalid maintenance node identity")
	}
	owner := s.maintenance.Owner
	if owner.Coordinator != "" && (!validUUID(owner.Coordinator) || owner.Master == "" || owner.Name == "") || s.maintenance.Run != "" && (!validUUID(s.maintenance.Run) || owner.Coordinator == "") || s.maintenance.Lease < 0 {
		return errors.New("invalid persisted maintenance ownership")
	}
	return nil
}

func validUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func (s *Server) activeJobs() bool {
	if s.busy {
		return true
	}
	for _, j := range s.jobs {
		if j.Status == "running" || j.Status == "updating" {
			return true
		}
	}
	return false
}

// Caller holds mu. Expiry never overrides an active or uncertain helper.
func (s *Server) authorizeMaintenance(owner, run string) error {
	if owner != "" && s.maintenance.Owner.Coordinator != "" && owner != s.maintenance.Owner.Coordinator {
		return errors.New("master registration conflict")
	}
	if run != "" {
		if owner == "" || owner != s.maintenance.Owner.Coordinator || run != s.maintenance.Run || s.maintenance.Lease <= time.Now().Unix() {
			return errors.New("maintenance ownership missing or expired")
		}
	} else if s.maintenance.Run != "" && (s.maintenance.Lease > time.Now().Unix() || s.activeJobs()) {
		return errors.New("host reserved for maintenance")
	}
	return nil
}

func (s *Server) maintenanceAction(w http.ResponseWriter, r *http.Request) {
	var req MaintenanceRequest
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid maintenance request", 400)
		return
	}
	if !validUUID(req.Claim.Coordinator) || req.Claim.Master == "" || len(req.Claim.Master) > 256 || req.Claim.Name == "" || len(req.Claim.Name) > 128 {
		http.Error(w, "invalid master claim", 400)
		return
	}
	if req.Run != "" {
		if !validUUID(req.Run) {
			http.Error(w, "invalid run identity", 400)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.maintenance
	same := next.Owner.Coordinator == req.Claim.Coordinator && next.Owner.Master == req.Claim.Master
	if next.Owner.Coordinator != "" && !same {
		http.Error(w, "master registration conflict", 409)
		return
	}
	switch req.Operation {
	case "register":
		if next.Owner == req.Claim {
			writeJSON(w, next)
			return
		}
		next.Owner = req.Claim
	case "acquire":
		if !same || req.Run == "" || s.activeJobs() || (next.Run != "" && next.Run != req.Run && next.Lease > time.Now().Unix()) {
			http.Error(w, "host busy or maintenance ownership unavailable", 409)
			return
		}
		next.Run, next.Lease = req.Run, time.Now().Add(2*time.Minute).Unix()
	case "renew":
		if !same || req.Run == "" || next.Run != req.Run {
			http.Error(w, "maintenance ownership lost", 409)
			return
		}
		next.Lease = time.Now().Add(2 * time.Minute).Unix()
	case "release":
		if !same || req.Run == "" || next.Run != req.Run || s.activeJobs() {
			http.Error(w, "maintenance still active or ownership lost", 409)
			return
		}
		next.Run, next.Lease = "", 0
	case "unregister":
		if !req.Confirmed || !same || next.Run != "" || s.activeJobs() {
			http.Error(w, "registration release requires confirmation and an idle host", 409)
			return
		}
		next.Owner = MasterClaim{}
	case "recover":
		if !req.Confirmed || !same || s.activeJobs() || next.Lease > time.Now().Unix() {
			http.Error(w, "recovery requires confirmed owner identity, expired lease and no active jobs", 409)
			return
		}
		next.Owner, next.Run, next.Lease = MasterClaim{}, "", 0
	default:
		http.Error(w, "unknown maintenance operation", 400)
		return
	}
	if err := s.saveState("maintenance.json", next); err != nil {
		http.Error(w, "cannot persist maintenance state", 500)
		return
	}
	s.maintenance = next
	writeJSON(w, next)
}

func (e *Engine) updateTargets(ctx context.Context) (string, []UpdateTarget, error) {
	hostID, err := e.capture(ctx, "info", "--format", "{{.ID}}")
	if err != nil || strings.TrimSpace(hostID) == "" {
		return "", nil, errors.New("cannot identify Docker host")
	}
	containers, err := e.containers(ctx)
	if err != nil {
		return "", nil, err
	}
	hostname, _ := os.Hostname()
	var targets []UpdateTarget
	type discovery struct {
		services map[string]composeService
		err      error
	}
	configs := map[string]discovery{}
	for _, c := range containers {
		role := c.Config.Labels[RoleLabel]
		if role != "agent" && role != "ui" {
			continue
		}
		r := UpdateTarget{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Role: role, Image: c.Image, Commit: c.Config.Labels["org.opencontainers.image.revision"], State: c.State.Status, Self: role == "agent" && len(hostname) >= 12 && strings.HasPrefix(c.ID, hostname)}
		if c.State.Health != nil {
			r.Health = c.State.Health.Status
		}
		if r.Self {
			if _, err := e.capture(ctx, "image", "inspect", c.Image); err != nil {
				r.Error = "Current agent image unavailable; restore it before updating"
			}
		}
		t, err := e.composeTarget(c)
		if err == nil {
			identity, _ := json.Marshal(struct {
				Target  composeTarget
				Service string
			}{t, c.Config.Labels[ServiceLabel]})
			r.Service = fmt.Sprintf("%x", sha256.Sum256(identity))
			key, _ := json.Marshal(t)
			config, ok := configs[string(key)]
			if !ok {
				config.services, config.err = e.composeServices(ctx, t)
				configs[string(key)] = config
			}
			err = config.err
			if err == nil {
				service, ok := config.services[c.Config.Labels[ServiceLabel]]
				if !ok || service.Build != nil {
					err = errors.New("management updates require a registry-based Compose service")
				}
			}
		}
		if err != nil {
			r.Error = "Update unavailable: check Compose files, environment and registry-based service"
		}
		targets = append(targets, r)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return strings.TrimSpace(hostID), targets, nil
}

func (s *Server) maintenanceInfo(w http.ResponseWriter, r *http.Request) {
	// Polling also reconciles detached helpers after an agent restart.
	s.mu.Lock()
	var ids []string
	for id, j := range s.jobs {
		if j.Status == "updating" {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.refresh(id)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var hostID string
	var targets []UpdateTarget
	var err error
	if r.URL.Query().Get("discovery") == "true" {
		hostID, targets, err = s.Engine.updateTargets(ctx)
	} else {
		hostID, err = s.Engine.capture(ctx, "info", "--format", "{{.ID}}")
	}
	if err != nil || strings.TrimSpace(hostID) == "" {
		http.Error(w, "maintenance discovery unavailable", 502)
		return
	}
	s.mu.Lock()
	info := MaintenanceInfo{Version: 1, HostID: strings.TrimSpace(hostID), Commit: buildinfo.Commit, State: s.maintenance, Busy: s.activeJobs(), Targets: targets}
	s.mu.Unlock()
	writeJSON(w, info)
}
