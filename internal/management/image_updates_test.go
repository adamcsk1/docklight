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
			_, err := io.WriteString(out, "docker-host\n")
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
	if inspects != 1 || snapshot.Version != 1 || snapshot.Hostname != "docker-host" || len(snapshot.Containers) != 2 || snapshot.Containers[0].Architecture != "arm" || snapshot.Containers[0].Variant != "v7" || snapshot.Containers[0].Error != "" || snapshot.Containers[0].Skip != "" {
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
					_, err := io.WriteString(out, "host")
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
