package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestDeleteUnusedValidation(t *testing.T) {
	for _, kind := range []string{"image", "volume", "network"} {
		valid := Action{Kind: kind, ID: "unused", Operation: "delete-unused", Confirmed: true}
		if err := ValidateAction(valid); err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*Action){
			func(a *Action) { a.Confirmed = false },
			func(a *Action) { a.ID = "" },
			func(a *Action) { a.ID = "other" },
			func(a *Action) { a.Scope = "stack" },
			func(a *Action) { a.Kind = "container" },
			func(a *Action) { a.Kind = "unknown" },
		} {
			a := valid
			change(&a)
			f := newFixture(t)
			if err := f.e.Execute(context.Background(), a, io.Discard); err == nil || len(f.calls) != 0 {
				t.Fatalf("unsafe action reached Docker: %+v", a)
			}
			s, err := NewServer(f.e, testToken)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(a)
			if w := agentRequest(s, "POST", "/v1/jobs", string(b), testToken); w.Code != 400 {
				t.Fatalf("agent accepted %+v: %d", a, w.Code)
			}
		}
	}
}

func TestDeleteUnusedSafetyAndResults(t *testing.T) {
	for _, kind := range []string{"image", "volume", "network"} {
		for _, scenario := range []struct {
			name    string
			want    []string
			totals  string
			wantErr bool
		}{
			{"success", []string{"a", "b"}, "2 deleted, 0 skipped, 0 failed, 0 unprocessed", false},
			{"new-resource", []string{"a", "b"}, "2 deleted, 0 skipped, 0 failed, 0 unprocessed", false},
			{"now-used", []string{"b"}, "1 deleted, 1 skipped, 0 failed, 0 unprocessed", false},
			{"second-now-used", []string{"a"}, "1 deleted, 1 skipped, 0 failed, 0 unprocessed", false},
			{"vanished", []string{"b"}, "1 deleted, 1 skipped, 0 failed, 0 unprocessed", false},
			{"partial-failure", []string{"a", "b"}, "1 deleted, 0 skipped, 1 failed, 0 unprocessed", true},
			{"recheck-failure", nil, "0 deleted, 0 skipped, 0 failed, 2 unprocessed", true},
			{"initial-failure", nil, "", true},
			{"initial-inventory-failure", nil, "", true},
			{"recheck-inventory-failure", nil, "0 deleted, 0 skipped, 0 failed, 2 unprocessed", true},
			{"cancel-before-recheck", nil, "0 deleted, 0 skipped, 0 failed, 2 unprocessed", true},
			{"cancel-after-recheck", nil, "0 deleted, 0 skipped, 0 failed, 2 unprocessed", true},
			{"cancel-during-delete", []string{"a"}, "0 deleted, 0 skipped, 1 failed, 1 unprocessed", true},
			{"cancel-between-deletes", []string{"a"}, "1 deleted, 0 skipped, 0 failed, 1 unprocessed", true},
			{"empty", nil, "0 deleted, 0 skipped, 0 failed, 0 unprocessed", false},
		} {
			t.Run(kind+"/"+scenario.name, func(t *testing.T) {
				f := newFixture(t)
				f.containers[0].State.Status = "exited"
				base := f.e.Run
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				snapshot := 0
				var removed []string
				f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
					if args[0] == "network" && args[1] == "inspect" && ((scenario.name == "cancel-before-recheck" && snapshot == 1) || (scenario.name == "cancel-after-recheck" && snapshot == 2)) {
						cancel()
					}
					if args[0] == "container" && args[1] == "ls" {
						snapshot++
						if scenario.name == "initial-failure" || (scenario.name == "recheck-failure" && snapshot > 1) {
							return errors.New("Docker unavailable")
						}
						if (scenario.name == "now-used" && snapshot > 1) || (scenario.name == "second-now-used" && snapshot > 2) {
							id := "a"
							if scenario.name == "second-now-used" {
								id = "b"
							}
							switch kind {
							case "image":
								f.containers[0].Image = id
							case "volume":
								f.containers[0].Mounts = append(f.containers[0].Mounts, struct{ Type, Name string }{"volume", id})
							case "network":
								f.containers[0].NetworkSettings.Networks[id] = struct{}{}
							}
						}
					}
					if args[0] == kind && args[1] == "inspect" {
						if scenario.name == "initial-inventory-failure" || (scenario.name == "recheck-inventory-failure" && snapshot > 1) {
							return errors.New("inventory unavailable")
						}
						items := []map[string]any{}
						if scenario.name != "empty" {
							items = append(items, map[string]any{"ID": "a", "Name": "a"}, map[string]any{"ID": "b", "Name": "b"})
							if scenario.name == "vanished" && snapshot > 1 {
								items = items[1:]
							}
							if scenario.name == "new-resource" && snapshot > 1 {
								items = append(items, map[string]any{"ID": "late", "Name": "late"})
							}
						}
						// Existing stopped-container references and built-in networks must never become candidates.
						switch kind {
						case "image":
							items = append(items, map[string]any{"ID": "sha256:" + strings.Repeat("b", 64)})
						case "volume":
							items = append(items, map[string]any{"Name": "used-volume"})
						case "network":
							items = append(items, map[string]any{"ID": "used", "Name": "used-network"}, map[string]any{"ID": "bridge", "Name": "bridge"}, map[string]any{"ID": "host", "Name": "host"}, map[string]any{"ID": "none", "Name": "none"})
						}
						return json.NewEncoder(out).Encode(items)
					}
					if len(args) > 1 && args[1] == "rm" {
						if args[0] != kind || len(args) != 3 {
							t.Fatalf("unsafe delete: %v", args)
						}
						removed = append(removed, args[2])
						if scenario.name == "cancel-during-delete" {
							cancel()
							return ctx.Err()
						}
						if scenario.name == "cancel-between-deletes" {
							cancel()
						}
						if scenario.name == "partial-failure" && args[2] == "a" {
							return errors.New("Docker refused removal")
						}
						return nil
					}
					return base(ctx, dir, out, args...)
				}
				var out bytes.Buffer
				err := f.e.Execute(ctx, Action{Kind: kind, ID: "unused", Operation: "delete-unused", Confirmed: true}, &out)
				if (err != nil) != scenario.wantErr || !reflect.DeepEqual(removed, scenario.want) || !strings.Contains(out.String(), scenario.totals) {
					t.Fatalf("removed=%v err=%v output=%s", removed, err, &out)
				}
				if scenario.name == "empty" && !strings.Contains(out.String(), "Nothing to remove") {
					t.Fatal(out.String())
				}
				if scenario.name == "partial-failure" && !strings.Contains(out.String(), "Failed "+kind+" a: Docker refused removal") {
					t.Fatal(out.String())
				}
			})
		}
	}
}

func TestCleanupJobSerializationAndPersistence(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failed"}[fail], func(t *testing.T) {
			f := newFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			base := f.e.Run
			f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
				if args[0] == "volume" && args[1] == "rm" {
					close(started)
					<-release
					if fail {
						return errors.New("removal refused")
					}
				}
				return base(ctx, dir, out, args...)
			}
			s, err := NewServer(f.e, testToken)
			if err != nil {
				t.Fatal(err)
			}
			body := `{"kind":"volume","id":"unused","operation":"delete-unused","confirmed":true}`
			w := agentRequest(s, "POST", "/v1/jobs", body, testToken)
			if w.Code != 202 {
				t.Fatal(w.Body.String())
			}
			var j Job
			if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
				t.Fatal(err)
			}
			<-started
			w = agentRequest(s, "POST", "/v1/jobs", `{"kind":"container","id":"`+testID+`","operation":"stop"}`, testToken)
			close(release)
			if w.Code != 409 {
				t.Fatalf("concurrent mutation accepted: %d", w.Code)
			}
			j = waitJob(t, s, j.ID)
			want := "success"
			if fail {
				want = "failed"
			}
			if j.Status != want || !strings.Contains(j.Output, "Cleanup totals:") {
				t.Fatalf("%+v", j)
			}
			restarted, err := NewServer(f.e, testToken)
			if err != nil {
				t.Fatal(err)
			}
			if got := waitJob(t, restarted, j.ID); !reflect.DeepEqual(got, j) {
				t.Fatalf("lost result: %+v", got)
			}
		})
	}
}
