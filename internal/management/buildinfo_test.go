package management

import (
	"encoding/json"
	"testing"

	"docklight/internal/buildinfo"
)

func TestInventoryReportsAgentBuildCommit(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	old := buildinfo.Commit
	buildinfo.Commit = "0123456789abcdef0123456789abcdef01234567"
	defer func() { buildinfo.Commit = old }()
	w := agentRequest(s, "GET", "/v1/inventory", "", testToken)
	var inv Inventory
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil || w.Code != 200 || inv.Commit != buildinfo.Commit {
		t.Fatalf("missing agent build commit: %s %v", w.Body.String(), err)
	}
}
