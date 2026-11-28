package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestImageSnapshotReadOnlyUsesDeployedID(t *testing.T) {
	f := newFixture(t)
	base := f.e.Run
	replica := f.containers[0]
	replica.ID = strings.Repeat("c", 64)
	replica.Name = "/web-2"
	f.containers = append(f.containers, replica)
	inspects := 0
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		switch {
		case args[0] == "info":
			_, err := io.WriteString(out, `{"Name":"docker-host"}`)
			return err
		case args[0] == "image" && args[1] == "inspect":
			inspects++
			if len(args) != 3 || args[2] != f.containers[0].Image {
				t.Fatalf("inspected mutable tag instead of deployed ID: %v", args)
			}
			return json.NewEncoder(out).Encode([]map[string]any{{"Id": f.containers[0].Image, "Os": "linux", "Architecture": "arm", "Variant": "v7", "RepoDigests": []string{"example@sha256:" + strings.Repeat("d", 64)}}})
		case reflect.DeepEqual(args[:min(2, len(args))], []string{"container", "ls"}), reflect.DeepEqual(args[:min(2, len(args))], []string{"container", "inspect"}):
			return base(ctx, dir, out, args...)
		default:
			t.Fatalf("unexpected/mutating command %v", args)
			return nil
		}
	}
	snapshot, err := f.e.ImageSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inspects != 1 || snapshot.Version != ImageSnapshotVersion || snapshot.Hostname != "docker-host" || len(snapshot.Containers) != 2 || snapshot.Containers[0].Architecture != "arm" || snapshot.Containers[0].Variant != "v7" || snapshot.Containers[0].Error != "" || snapshot.Containers[0].Skip != "" {
		t.Fatalf("wrong snapshot %+v", snapshot)
	}
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", testToken} {
		r := httptest.NewRequest("GET", "/v1/image-updates", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if token == "" && w.Code != 401 || token != "" && w.Code != 200 {
			t.Fatalf("endpoint authentication: %d", w.Code)
		}
		if token != "" {
			var got ImageSnapshot
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if got.NodeID == "" {
				t.Fatal("missing persistent daemon identity")
			}
		}
	}
}

func TestImageSnapshotSkipsAndMetadataFailure(t *testing.T) {
	for _, kind := range []string{"pinned", "id", "disabled", "helper", "local", "failed"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			base := f.e.Run
			switch kind {
			case "pinned":
				f.containers[0].Config.Image += "@sha256:" + strings.Repeat("d", 64)
			case "id":
				f.containers[0].Config.Image = f.containers[0].Image
			case "disabled":
				f.containers[0].Config.Labels["io.docklight.updates"] = "false"
			case "helper":
				f.containers[0].Config.Labels[RoleLabel] = "updater"
			}
			f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
				if args[0] == "info" {
					_, err := io.WriteString(out, `{"Name":"host"}`)
					return err
				}
				if args[0] == "image" {
					if kind == "failed" {
						return errors.New("private registry token")
					}
					if kind != "local" {
						t.Fatal("skipped image inspected")
					}
					return json.NewEncoder(out).Encode([]map[string]any{{"Id": f.containers[0].Image, "Os": "linux", "Architecture": "amd64"}})
				}
				return base(ctx, dir, out, args...)
			}
			snapshot, err := f.e.ImageSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			result := snapshot.Containers[0]
			if kind == "failed" {
				if result.Error == "" || strings.Contains(result.Error, "token") {
					t.Fatal("unsafe metadata error")
				}
			} else if result.Skip == "" {
				t.Fatal("untracked image not marked")
			}
		})
	}
}

func TestImageSnapshotContainerdPlatformManifests(t *testing.T) {
	for _, mode := range []string{"index", "manifest", "missing-manifest", "missing-descriptors", "missing-platform", "wrong-media-type"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			base := f.e.Run
			selected := "sha256:" + strings.Repeat("c", 64)
			descriptor := &imageDescriptor{Digest: selected, MediaType: "application/vnd.oci.image.manifest.v1+json", Platform: &struct{ OS, Architecture, Variant string }{OS: "linux", Architecture: "arm", Variant: "v7"}}
			f.containers[0].ImageManifestDescriptor = descriptor
			target := &imageDescriptor{Digest: f.containers[0].Image, MediaType: "application/vnd.oci.image.index.v1+json"}
			switch mode {
			case "manifest":
				target.MediaType = "application/vnd.docker.distribution.manifest.v2+json"
				target.Digest = selected
				f.containers[0].Image = selected
				f.containers[0].ImageManifestDescriptor = nil
			case "missing-manifest":
				f.containers[0].ImageManifestDescriptor = nil
			case "missing-descriptors":
				f.containers[0].ImageManifestDescriptor = nil
				target = nil
			case "missing-platform":
				descriptor.Platform = nil
			case "wrong-media-type":
				descriptor.MediaType = target.MediaType
			}
			f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
				if args[0] == "info" {
					_, err := io.WriteString(out, `{"Name":"host","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]]}`)
					return err
				}
				if args[0] == "image" {
					if len(args) != 3 || args[2] != f.containers[0].Image {
						t.Fatal("inspected a mutable tag instead of deployed image")
					}
					return json.NewEncoder(out).Encode([]map[string]any{{"Id": f.containers[0].Image, "Os": "linux", "Architecture": "amd64", "RepoDigests": []string{"registry.example/web@" + f.containers[0].Image}, "Descriptor": target}})
				}
				return base(ctx, dir, out, args...)
			}
			snapshot, err := f.e.ImageSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			result := snapshot.Containers[0]
			valid := mode == "index" || mode == "manifest"
			if valid {
				if result.Error != "" || result.ManifestDigest != selected || result.ImageID != f.containers[0].Image || result.DeployedDigest() != selected {
					t.Fatalf("wrong deployed manifest: %+v", result)
				}
				if mode == "index" && (result.Architecture != "arm" || result.Variant != "v7") {
					t.Fatal("default image-inspect platform overrode container's actual platform")
				}
			} else if result.Error == "" {
				t.Fatalf("ambiguous containerd metadata accepted: %+v", result)
			}
		})
	}
}

func TestImageSnapshotRejectsUnknownStoreMetadata(t *testing.T) {
	f := newFixture(t)
	base := f.e.Run
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if args[0] == "info" {
			_, err := io.WriteString(out, "malformed private daemon metadata")
			return err
		}
		if args[0] == "image" {
			t.Fatal("inspected image without identifying image store")
		}
		return base(ctx, dir, out, args...)
	}
	if _, err := f.e.ImageSnapshot(context.Background()); err == nil || err.Error() != "Docker image store metadata unavailable" {
		t.Fatalf("unknown image store accepted or metadata leaked: %v", err)
	}
}
