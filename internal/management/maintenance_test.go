package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func maintenanceRequest(t *testing.T, s *Server, op string, claim MasterClaim, run string, confirmed bool) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(MaintenanceRequest{Operation: op, Claim: claim, Run: run, Confirmed: confirmed})
	return agentRequest(s, "POST", "/v1/maintenance", string(b), testToken)
}
func newMaintenanceServer(t *testing.T) (*fixture, *Server) {
	t.Helper()
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}

func TestMasterRegistrationPersistsAndConflicts(t *testing.T) {
	f, s := newMaintenanceServer(t)
	claim := MasterClaim{Coordinator: uuid.NewString(), Master: "main-host", Name: "Main"}
	id := s.maintenance.NodeID
	for i := 0; i < 2; i++ {
		if w := maintenanceRequest(t, s, "register", claim, "", false); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	for _, other := range []MasterClaim{{Coordinator: uuid.NewString(), Master: claim.Master, Name: "Other"}, {Coordinator: claim.Coordinator, Master: "different", Name: "Other"}} {
		if w := maintenanceRequest(t, s, "register", other, "", false); w.Code != 409 {
			t.Fatalf("conflicting owner accepted: %d", w.Code)
		}
	}
	restarted, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.maintenance.NodeID != id || restarted.maintenance.Owner != claim {
		t.Fatalf("lost registration: %+v", restarted.maintenance)
	}
	if w := maintenanceRequest(t, restarted, "unregister", claim, "", false); w.Code != 409 {
		t.Fatal("unconfirmed unregister accepted")
	}
	if w := maintenanceRequest(t, restarted, "unregister", claim, "", true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if restarted.maintenance.Owner.Coordinator != "" || restarted.maintenance.NodeID != id {
		t.Fatal("unregister changed node identity")
	}
}

func TestConcurrentMasterClaimsHaveOneWinner(t *testing.T) {
	_, s := newMaintenanceServer(t)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := json.Marshal(MaintenanceRequest{Operation: "register", Claim: MasterClaim{Coordinator: uuid.NewString(), Master: "main", Name: "Main"}})
			codes <- agentRequest(s, "POST", "/v1/maintenance", string(b), testToken).Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("%v", counts)
	}
}

func TestMaintenanceLeaseExpiryAndRecovery(t *testing.T) {
	_, s := newMaintenanceServer(t)
	claim := MasterClaim{Coordinator: uuid.NewString(), Master: "main", Name: "Main"}
	run := uuid.NewString()
	for _, op := range []string{"register", "acquire", "renew"} {
		if w := maintenanceRequest(t, s, op, claim, run, false); w.Code != 200 {
			t.Fatalf("%s: %s", op, w.Body.String())
		}
	}
	if err := s.authorizeMaintenance("", ""); err == nil {
		t.Fatal("ordinary mutation bypassed live reservation")
	}
	if err := s.authorizeMaintenance(claim.Coordinator, run); err != nil {
		t.Fatal(err)
	}
	if w := maintenanceRequest(t, s, "recover", claim, "", true); w.Code != 409 {
		t.Fatal("live master recovered")
	}
	s.maintenance.Lease = time.Now().Add(-time.Second).Unix()
	if err := s.authorizeMaintenance(claim.Coordinator, run); err == nil {
		t.Fatal("expired owner mutated host")
	}
	s.jobs[uuid.NewString()] = &storedJob{Job: Job{Status: "updating"}}
	if err := s.authorizeMaintenance("", ""); err == nil {
		t.Fatal("expired lease bypassed uncertain helper")
	}
	for _, op := range []string{"release", "recover", "acquire", "unregister"} {
		if w := maintenanceRequest(t, s, op, claim, run, true); w.Code != 409 {
			t.Fatalf("%s ignored active helper", op)
		}
	}
	for id := range s.jobs {
		delete(s.jobs, id)
	}
	if w := maintenanceRequest(t, s, "recover", claim, "", false); w.Code != 409 {
		t.Fatal("unconfirmed recovery accepted")
	}
	if w := maintenanceRequest(t, s, "recover", claim, "", true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if s.maintenance.Run != "" || s.maintenance.Owner.Coordinator != "" {
		t.Fatal("recovery did not clear stale ownership")
	}
}

func TestMaintenanceJobReplayAndRestart(t *testing.T) {
	f, s := newMaintenanceServer(t)
	claim := MasterClaim{Coordinator: uuid.NewString(), Master: "main", Name: "Main"}
	run, jobID := uuid.NewString(), uuid.NewString()
	for _, op := range []string{"register", "acquire"} {
		if w := maintenanceRequest(t, s, op, claim, run, false); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	a := Action{Kind: "container", ID: testID, Operation: "stop"}
	b, _ := json.Marshal(a)
	request := func(server *Server, body string, owner, runID, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("X-Docklight-Coordinator", owner)
		r.Header.Set("X-Docklight-Run", runID)
		r.Header.Set("X-Docklight-Job", id)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		owner, run, id string
		code           int
	}{{"", "", "", 409}, {uuid.NewString(), run, jobID, 409}, {claim.Coordinator, run, "invalid", 400}, {claim.Coordinator, run, "", 400}, {claim.Coordinator, uuid.NewString(), jobID, 409}} {
		if w := request(s, string(b), tc.owner, tc.run, tc.id); w.Code != tc.code {
			t.Fatalf("bad ownership accepted: %d %s", w.Code, w.Body.String())
		}
	}
	if w := request(s, string(b), claim.Coordinator, run, jobID); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	j := waitJob(t, s, jobID)
	if j.Status != "success" {
		t.Fatalf("%+v", j)
	}
	restarted, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if w := request(restarted, string(b), claim.Coordinator, run, jobID); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	stops := 0
	for _, call := range f.calls {
		if len(call) > 1 && call[1] == "stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("replayed mutation %d times", stops)
	}
	a.Operation = "start"
	b, _ = json.Marshal(a)
	if w := request(restarted, string(b), claim.Coordinator, run, jobID); w.Code != 409 {
		t.Fatal("job ID reused for different action")
	}
	if w := maintenanceRequest(t, restarted, "release", claim, run, false); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestMaintenanceAuthValidationAndPersistenceFailure(t *testing.T) {
	f, s := newMaintenanceServer(t)
	for _, path := range []string{"/v1/maintenance", "/v1/jobs"} {
		if w := agentRequest(s, "POST", path, `{}`, ""); w.Code != 401 {
			t.Fatal("unauthenticated maintenance")
		}
	}
	for _, body := range []string{`{}`, `{} {}`, `{"Operation":"register","extra":true}`, strings.Repeat("x", 5000)} {
		if w := agentRequest(s, "POST", "/v1/maintenance", body, testToken); w.Code != 400 {
			t.Fatalf("invalid body accepted: %d", w.Code)
		}
	}
	claim := MasterClaim{Coordinator: uuid.NewString(), Master: "main", Name: "Main"}
	if w := maintenanceRequest(t, s, "unknown", claim, "", false); w.Code != 400 {
		t.Fatal("unknown operation")
	}
	if err := os.Remove(filepath.Join(f.e.DataDir, "maintenance.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.e.DataDir); err != nil {
		t.Fatal(err)
	}
	if w := maintenanceRequest(t, s, "register", claim, "", false); w.Code != 500 {
		t.Fatal("unpersisted owner accepted")
	}
	if s.maintenance.Owner.Coordinator != "" {
		t.Fatal("failed save changed owner")
	}
}

func TestMaintenanceDiscoveryAndCorruptState(t *testing.T) {
	f, s := newMaintenanceServer(t)
	f.containers[0].Config.Labels[RoleLabel] = "ui"
	base := f.e.Run
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if args[0] == "info" {
			_, err := io.WriteString(out, "docker-host")
			return err
		}
		return base(ctx, dir, out, args...)
	}
	w := agentRequest(s, "GET", "/v1/maintenance?discovery=true", "", testToken)
	var info MaintenanceInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || w.Code != 200 || info.Version != 1 || info.HostID != "docker-host" || len(info.Targets) != 1 || info.Targets[0].Service == "" {
		t.Fatalf("bad discovery: %s %v", w.Body.String(), err)
	}
	f.failConfig = true
	// Cheap ownership polling must not execute Compose discovery on idle hosts.
	f.calls = nil
	if cheap := agentRequest(s, "GET", "/v1/maintenance", "", testToken); cheap.Code != 200 {
		t.Fatal(cheap.Body.String())
	}
	for _, call := range f.calls {
		if call[0] == "compose" {
			t.Fatal("idle ownership poll ran Compose")
		}
	}
	w = agentRequest(s, "GET", "/v1/maintenance?discovery=true", "", testToken)
	if strings.Contains(w.Body.String(), "secret config diagnostic") || !strings.Contains(w.Body.String(), "Update unavailable") {
		t.Fatal("unsafe Compose diagnostics")
	}
	for _, content := range []string{"invalid", `{"NodeID":"not-uuid"}`, `{"NodeID":"` + uuid.NewString() + `","Owner":{"Coordinator":"broken"}}`} {
		if err := os.WriteFile(filepath.Join(f.e.DataDir, "maintenance.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewServer(f.e, testToken); err == nil {
			t.Fatal("corrupt ownership silently reset")
		}
	}
}
