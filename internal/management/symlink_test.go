package management

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" && os.Getenv("DOCKLIGHT_REQUIRE_SYMLINKS") != "1" {
			t.Skip("enable Windows Developer Mode or run Linux container tests: ", err)
		}
		t.Fatal("symlink support required: ", err)
	}
}

func TestComposeSymlinkEscapeRejected(t *testing.T) {
	for _, label := range []string{WorkingDirLabel, FilesLabel, EnvironmentFileLabel} {
		for _, chained := range []bool{false, true} {
			name := label
			if chained {
				name += "/chained"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				outside := t.TempDir()
				target := filepath.Join(outside, "compose.yml")
				if err := os.WriteFile(target, []byte("services: {}"), 0600); err != nil {
					t.Fatal(err)
				}
				if label == WorkingDirLabel {
					target = outside
				}
				link := filepath.Join(f.e.Root, "escape")
				testSymlink(t, target, link)
				if chained {
					chain := filepath.Join(f.e.Root, "chain")
					testSymlink(t, link, chain)
					link = chain
				}
				f.containers[0].Config.Labels[label] = link
				for _, operation := range []string{"recreate", "upgrade", "build"} {
					if err := f.e.Execute(context.Background(), Action{Kind: "container", ID: testID, Operation: operation, Scope: "service"}, io.Discard); err == nil {
						t.Fatalf("%s accepted escaping %s", operation, label)
					}
				}
				for _, call := range f.calls {
					if call[0] == "compose" {
						t.Fatalf("unsafe path reached Compose: %v", call)
					}
				}
			})
		}
	}
}

func TestComposeSymlinksWithinRootAllowed(t *testing.T) {
	f := newFixture(t)
	original := f.containers[0].Config.Labels[FilesLabel]
	link := filepath.Join(f.e.Root, "linked.yml")
	testSymlink(t, original, link)
	f.containers[0].Config.Labels[FilesLabel] = link
	f.containers[0].Config.Labels[EnvironmentFileLabel] = link
	rootLink := filepath.Join(t.TempDir(), "root-link")
	testSymlink(t, f.e.Root, rootLink)
	f.e.Root = rootLink
	target, err := f.e.composeTarget(f.containers[0])
	if err != nil || target.Files[0] != original || target.EnvFiles[0] != original {
		t.Fatalf("valid symlinks rejected: %+v %v", target, err)
	}
}
