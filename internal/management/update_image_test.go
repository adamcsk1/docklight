package management

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestMissingHelperImageFailsBeforeLaunch(t *testing.T) {
	f := newFixture(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if len(hostname) < 12 {
		t.Skip("default container identity fixture needs a long hostname")
	}
	f.containers[0].Config.Labels[RoleLabel] = "ui"
	agent := f.containers[0]
	agent.ID = hostname + strings.Repeat("d", 64)
	agent.Config.Labels = map[string]string{RoleLabel: "agent"}
	f.containers = append(f.containers, agent)
	base := f.e.Run
	launched := false
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if args[0] == "image" && args[1] == "inspect" {
			return errors.New("image gone")
		}
		if args[0] == "run" {
			launched = true
		}
		return base(ctx, dir, out, args...)
	}
	err = f.e.Update(context.Background(), Action{Kind: "container", ID: testID, Operation: "update", Confirmed: true}, io.Discard)
	var uncertain helperLaunchError
	if err == nil || errors.As(err, &uncertain) || !strings.Contains(err.Error(), "current agent image unavailable") || launched {
		t.Fatalf("missing image left uncertain helper: %v launched=%v", err, launched)
	}
}
