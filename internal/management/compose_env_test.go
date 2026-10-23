package management

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestComposeEnvironmentFilesPreserveOrder(t *testing.T) {
	for _, operation := range []string{"recreate", "upgrade", "build"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			f.buildable = map[string]bool{"web": operation == "build"}
			files := []string{filepath.Join(f.e.Root, "base.env"), filepath.Join(f.e.Root, "production.env")}
			for _, file := range files {
				if err := os.WriteFile(file, []byte("VALUE=production\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			f.containers[0].Config.Labels[EnvironmentFileLabel] = files[0] + "," + files[1]
			if err := f.e.Execute(context.Background(), Action{Kind: "container", ID: testID, Operation: operation, Scope: "service"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			for _, call := range f.calls {
				if call[0] != "compose" {
					continue
				}
				var got []string
				for i, arg := range call {
					if arg == "--env-file" {
						got = append(got, call[i+1])
					}
				}
				if !reflect.DeepEqual(got, files) {
					t.Fatalf("environment files %v, want %v", got, files)
				}
			}
		})
	}
}

func TestComposeEnvironmentFilesRejectUnsafePaths(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.env")
	if err := os.WriteFile(outside, []byte("VALUE=outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative.env", outside, f.e.Root, filepath.Join(f.e.Root, "missing.env")} {
		f.containers[0].Config.Labels[EnvironmentFileLabel] = path
		if _, err := f.e.composeTarget(f.containers[0]); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
