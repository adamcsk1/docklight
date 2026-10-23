package management

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateResultRetriesAfterPersistenceFailure(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	j := &storedJob{Job: Job{ID: "eab47565-129a-432d-a773-65a400ebc0f1", Status: "updating", Action: Action{ID: testID}}, Helper: "helper"}
	if err := s.save(j); err != nil {
		t.Fatal(err)
	}
	s.jobs[j.ID] = j
	removed := false
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if args[0] == "logs" {
			_, err := io.WriteString(out, "updated")
			return err
		}
		if args[0] == "container" {
			removed = true
			return nil
		}
		if args[2] == "{{json .State}}" {
			return json.NewEncoder(out).Encode(map[string]any{"Status": "exited", "ExitCode": 0})
		}
		c := f.containers[0]
		c.Config.Labels[RoleLabel] = "updater"
		c.Config.Labels["io.docklight.update.target"] = testID
		c.State.Status = "exited"
		return json.NewEncoder(out).Encode(c)
	}
	data := f.e.DataDir
	// A regular file in place of the data directory reliably fails writes on all platforms.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f.e.DataDir = blocked
	s.refresh(j)
	if j.Status != "updating" || removed {
		t.Fatal("unpersisted result finalized or helper removed")
	}
	f.e.DataDir = data
	restarted, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.jobs[j.ID].Status != "updating" {
		t.Fatal("restart lost unresolved update")
	}
	s.refresh(j)
	if j.Status != "success" || !removed {
		t.Fatal("result persistence was not retried")
	}
	restarted, err = NewServer(f.e, testToken)
	if err != nil || restarted.jobs[j.ID].Status != "success" {
		t.Fatalf("terminal result not durable: %v", err)
	}
}
