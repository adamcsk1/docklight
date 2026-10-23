package management

import (
	"context"
	"io"
	"slices"
	"testing"
	"time"
)

func TestInventorySurvivesComposeDiscoveryTimeout(t *testing.T) {
	f := newFixture(t)
	base := f.e.Run
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if slices.Contains(args, "config") {
			<-ctx.Done()
			return ctx.Err()
		}
		return base(ctx, dir, out, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	inv, err := f.e.Inventory(ctx)
	if err != nil || len(inv.Containers) != 1 || len(inv.Resources) == 0 || inv.Containers[0].BuildError == "" {
		t.Fatalf("discovery timeout lost basic inventory: %+v %v", inv, err)
	}
}

func TestResourceDeletionNeverDiscoversCompose(t *testing.T) {
	f := newFixture(t)
	base := f.e.Run
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if args[0] == "compose" {
			t.Fatal("resource deletion ran Compose")
		}
		return base(ctx, dir, out, args...)
	}
	if err := f.e.Execute(context.Background(), Action{Kind: "volume", ID: "unused-volume", Operation: "delete", Confirmed: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Execute(context.Background(), Action{Kind: "volume", ID: "used-volume", Operation: "delete", Confirmed: true}, io.Discard); err == nil {
		t.Fatal("reference check bypassed")
	}
}
