package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token-with-at-least-32-characters"

var testID = strings.Repeat("a", 64)

type fixture struct {
	e          *Engine
	containers []inspectContainer
	mu         sync.Mutex
	calls      [][]string
	failPull   bool
	override   map[string]map[string]map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "compose.yml")
	if err := os.WriteFile(file, []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	f := &fixture{e: &Engine{Root: root, DataDir: t.TempDir(), Timeout: time.Second}}
	c := inspectContainer{ID: testID, Name: "/web", Image: "sha256:" + strings.Repeat("b", 64)}
	c.Config.Image = "example:latest"
	c.Config.Labels = map[string]string{ProjectLabel: "web", ServiceLabel: "web", WorkingDirLabel: root, FilesLabel: file}
	c.State.Status = "running"
	c.Mounts = append(c.Mounts, struct{ Type, Name string }{"volume", "used-volume"})
	c.NetworkSettings.Networks = map[string]struct{}{"used-network": {}}
	f.containers = []inspectContainer{c}
	f.e.Run = f.run
	return f
}
func (f *fixture) run(ctx context.Context, dir string, out io.Writer, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
	write := func(v any) error { return json.NewEncoder(out).Encode(v) }
	if reflect.DeepEqual(args[:min(2, len(args))], []string{"container", "ls"}) {
		_, err := io.WriteString(out, testID)
		return err
	}
	if reflect.DeepEqual(args[:min(2, len(args))], []string{"container", "inspect"}) {
		return write(f.containers)
	}
	if len(args) > 1 && args[1] == "ls" {
		_, err := io.WriteString(out, "unused used")
		return err
	}
	if len(args) > 1 && args[1] == "inspect" {
		switch args[0] {
		case "image":
			return write([]map[string]any{{"ID": f.containers[0].Image, "RepoTags": []string{"example:latest"}}, {"ID": "unused-image", "RepoTags": []string{"unused:latest"}}})
		case "volume":
			return write([]map[string]string{{"Name": "used-volume"}, {"Name": "unused-volume"}})
		case "network":
			return write([]map[string]string{{"ID": "used-network-id", "Name": "used-network"}, {"ID": "unused-network-id", "Name": "unused-network"}, {"ID": "bridge-id", "Name": "bridge"}})
		}
	}
	if args[0] == "compose" {
		for i, arg := range args {
			if arg == "pull" && f.failPull {
				return errors.New("registry unavailable")
			}
			if arg == "-f" && strings.Contains(args[i+1], "recreate-") {
				b, err := os.ReadFile(args[i+1])
				if err != nil {
					return err
				}
				if err = json.Unmarshal(b, &f.override); err != nil {
					return err
				}
			}
		}
	}
	_, err := io.WriteString(out, "completed\n")
	return err
}

func TestRecreatePinsExactImageAndNeverPullsBuildsOrDowns(t *testing.T) {
	f := newFixture(t)
	if err := f.e.Execute(context.Background(), Action{Kind: "container", ID: testID, Operation: "recreate", Scope: "service"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := f.override["services"]["web"]; got["image"] != f.containers[0].Image || got["pull_policy"] != "never" {
		t.Fatalf("wrong override: %#v", got)
	}
	for _, call := range f.calls {
		for _, arg := range call {
			if arg == "pull" || arg == "down" || arg == "--build" {
				t.Fatalf("unsafe recreate: %v", call)
			}
		}
	}
	last := strings.Join(f.calls[len(f.calls)-1], " ")
	for _, flag := range []string{"--force-recreate", "--no-build", "--pull never", "--no-deps"} {
		if !strings.Contains(last, flag) {
			t.Fatalf("missing %s: %s", flag, last)
		}
	}
	// Compose records the now-removed override; subsequent actions must still work.
	f.containers[0].Config.Labels[FilesLabel] += "," + filepath.Join(f.e.DataDir, "recreate-old.json")
	if err := f.e.Execute(context.Background(), Action{Kind: "container", ID: testID, Operation: "upgrade", Scope: "service"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradePullFailureNeverRunsUp(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "pull-fails"}[fail], func(t *testing.T) {
			f := newFixture(t)
			f.failPull = fail
			err := f.e.Execute(context.Background(), Action{Kind: "container", ID: testID, Operation: "upgrade", Scope: "service"}, io.Discard)
			if (err != nil) != fail {
				t.Fatalf("unexpected error: %v", err)
			}
			var steps []string
			for _, call := range f.calls {
				if call[0] == "compose" {
					for _, arg := range call {
						if arg == "pull" || arg == "up" {
							steps = append(steps, arg)
						}
					}
				}
			}
			want := []string{"pull", "up"}
			if fail {
				want = []string{"pull"}
			}
			if !reflect.DeepEqual(steps, want) {
				t.Fatalf("steps %v", steps)
			}
		})
	}
}

func TestProtectedAndUnsafeActionsNeverMutate(t *testing.T) {
	cases := []struct {
		name   string
		action Action
		modify func(*fixture)
	}{
		{"unknown action", Action{Kind: "container", ID: testID, Operation: "exec"}, nil},
		{"delete without confirmation", Action{Kind: "container", ID: testID, Operation: "delete"}, nil},
		{"running deletion", Action{Kind: "container", ID: testID, Operation: "delete", Confirmed: true}, nil},
		{"partial ID", Action{Kind: "container", ID: testID[:12], Operation: "stop"}, nil},
		{"protected UI", Action{Kind: "container", ID: testID, Operation: "stop"}, func(f *fixture) { f.containers[0].Config.Labels[RoleLabel] = "ui" }},
		{"protected agent", Action{Kind: "container", ID: testID, Operation: "recreate", Scope: "stack"}, func(f *fixture) { f.containers[0].Config.Labels[RoleLabel] = "agent" }},
		{"standalone recreate", Action{Kind: "container", ID: testID, Operation: "recreate", Scope: "service"}, func(f *fixture) { delete(f.containers[0].Config.Labels, ProjectLabel) }},
		{"out of root", Action{Kind: "container", ID: testID, Operation: "upgrade", Scope: "service"}, func(f *fixture) { f.containers[0].Config.Labels[WorkingDirLabel] = t.TempDir() }},
		{"no scope recreate", Action{Kind: "container", ID: testID, Operation: "recreate"}, nil},
		{"used image", Action{Kind: "image", ID: "sha256:" + strings.Repeat("b", 64), Operation: "delete", Confirmed: true}, nil},
		{"stopped volume reference", Action{Kind: "volume", ID: "used-volume", Operation: "delete", Confirmed: true}, func(f *fixture) { f.containers[0].State.Status = "exited" }},
		{"used network", Action{Kind: "network", ID: "used-network-id", Operation: "delete", Confirmed: true}, nil},
		{"built-in network", Action{Kind: "network", ID: "bridge-id", Operation: "delete", Confirmed: true}, nil},
		{"resource injection", Action{Kind: "volume", ID: "--force", Operation: "delete", Confirmed: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.modify != nil {
				tc.modify(f)
			}
			if err := f.e.Execute(context.Background(), tc.action, io.Discard); err == nil {
				t.Fatal("unsafe action accepted")
			}
			for _, call := range f.calls {
				if call[0] == "compose" || len(call) > 1 && (call[1] == "rm" || call[1] == "stop") {
					t.Fatalf("mutation before validation: %v", call)
				}
			}
		})
	}
}

func TestUnusedDeletionAndStoppedContainerPreserveVolumes(t *testing.T) {
	for kind, id := range map[string]string{"image": "unused-image", "volume": "unused-volume", "network": "unused-network-id", "container": testID} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.containers[0].State.Status = "exited"
			if err := f.e.Execute(context.Background(), Action{Kind: kind, ID: id, Operation: "delete", Confirmed: true}, io.Discard); err != nil {
				t.Fatal(err)
			}
			want := []string{kind, "rm", id}
			got := f.calls[len(f.calls)-1]
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
}

func TestServiceAndStackScopeProtectionAndReplicaImages(t *testing.T) {
	f := newFixture(t)
	other := f.containers[0]
	other.ID = strings.Repeat("c", 64)
	other.Config.Labels = map[string]string{}
	for k, v := range f.containers[0].Config.Labels {
		other.Config.Labels[k] = v
	}
	other.Config.Labels[ServiceLabel] = "other"
	other.Config.Labels[RoleLabel] = "ui"
	f.containers = append(f.containers, other)
	if err := f.e.Execute(context.Background(), Action{Kind: "container", ID: testID, Operation: "stop", Scope: "service"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[len(f.calls)-1]; !reflect.DeepEqual(got, []string{"container", "stop", testID}) {
		t.Fatalf("wrong service target: %v", got)
	}
	if err := f.e.Execute(context.Background(), Action{Kind: "container", ID: testID, Operation: "stop", Scope: "stack"}, io.Discard); err == nil {
		t.Fatal("stack protection bypassed")
	}
	delete(f.containers[1].Config.Labels, RoleLabel)
	f.containers[1].Config.Labels[ServiceLabel] = "web"
	f.containers[1].Image = "different-image"
	if err := f.e.Execute(context.Background(), Action{Kind: "container", ID: testID, Operation: "recreate", Scope: "service"}, io.Discard); err == nil {
		t.Fatal("mixed replica images accepted")
	}
}

func agentRequest(s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestAgentAuthAndStrictRequests(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong", testToken + "x"} {
		w := agentRequest(s, "GET", "/v1/inventory", "", token)
		if w.Code != 401 {
			t.Fatalf("unauthenticated: %d", w.Code)
		}
	}
	if len(f.calls) > 0 {
		t.Fatal("Docker called before auth")
	}
	for _, body := range []string{`{"kind":"container","id":"x","operation":"exec"}`, `{"kind":"volume","id":"x","operation":"delete","confirmed":true,"shell":"id"}`, `{} {}`, strings.Repeat("x", 5000)} {
		w := agentRequest(s, "POST", "/v1/jobs", body, testToken)
		if w.Code != 400 {
			t.Fatalf("bad payload accepted: %d", w.Code)
		}
	}
	if w := agentRequest(s, "GET", "/v1/inventory", "", testToken); w.Code != 200 {
		t.Fatalf("inventory: %d %s", w.Code, w.Body.String())
	}
}

func waitJob(t *testing.T, s *Server, id string) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w := agentRequest(s, "GET", "/v1/jobs/"+id, "", testToken)
		var j Job
		if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		if j.Status != "running" {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job never completed")
	return Job{}
}
func TestJobsSerializePersistAndResumeAfterRestart(t *testing.T) {
	f := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	run := f.e.Run
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if len(args) > 1 && args[1] == "stop" {
			close(started)
			<-release
		}
		return run(ctx, dir, out, args...)
	}
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"kind":"container","id":"` + testID + `","operation":"stop"}`
	w := agentRequest(s, "POST", "/v1/jobs", body, testToken)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var j Job
	_ = json.Unmarshal(w.Body.Bytes(), &j)
	<-started
	if w = agentRequest(s, "POST", "/v1/jobs", body, testToken); w.Code != 409 {
		t.Fatalf("concurrent mutation accepted: %d", w.Code)
	}
	close(release)
	j = waitJob(t, s, j.ID)
	if j.Status != "success" || !strings.Contains(j.Output, "completed") {
		t.Fatalf("job: %#v", j)
	}
	restarted, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, restarted, j.ID); got.Status != "success" {
		t.Fatalf("lost result: %#v", got)
	}
	s.mu.Lock()
	s.jobs[j.ID].Status = "running"
	if err = s.save(s.jobs[j.ID]); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	restarted, err = NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, restarted, j.ID); got.Status != "failed" || !strings.Contains(got.Output, "outcome unknown") {
		t.Fatalf("stale job: %#v", got)
	}
}

func TestJobOutputBoundedAndShellQuoting(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	s.jobs["id"] = &storedJob{}
	b := bytes.Repeat([]byte("x"), maxOutput*2)
	if _, err = (jobWriter{s, "id"}).Write(b); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs["id"].Output) > maxOutput+100 {
		t.Fatal("unbounded output")
	}
	if shellQuote("a'b") != "'a'\"'\"'b'" {
		t.Fatal("unsafe shell quoting")
	}
}

func TestComposeSymlinkEscapeRejected(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "compose.yml")
	if err := os.WriteFile(outside, []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.e.Root, "escape.yml")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable: ", err)
	}
	f.containers[0].Config.Labels[FilesLabel] = link
	if _, err := f.e.composeTarget(f.containers[0]); err == nil {
		t.Fatal("symlink escaped root")
	}
}

func TestManagementUpdateUsesDetachedCurrentAgentImageAndTargetOnly(t *testing.T) {
	f := newFixture(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if len(hostname) < 12 {
		t.Skip("hostname too short for container identification fixture")
	}
	agent := f.containers[0]
	agent.ID = hostname + strings.Repeat("d", 64)
	agent.Image = "sha256:current-agent-image"
	agent.Config.Labels = map[string]string{RoleLabel: "agent"}
	f.containers[0].Config.Labels[RoleLabel] = "ui"
	f.containers = append(f.containers, agent)
	base := f.e.Run
	var launch []string
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if len(args) > 4 && args[0] == "container" && args[1] == "ls" {
			return nil
		}
		if args[0] == "run" {
			launch = append([]string{}, args...)
			return nil
		}
		return base(ctx, dir, out, args...)
	}
	a := Action{Kind: "container", ID: testID, Operation: "update", Confirmed: true}
	if err = f.e.Update(context.Background(), a, io.Discard); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(launch, " ")
	for _, required := range []string{"run -d", "--volumes-from " + agent.ID + ":ro", "--entrypoint /bin/sh " + agent.Image, "io.docklight.role=updater", "'pull' '--policy' 'always' 'web'", "'--no-deps'", "'--pull' 'never'"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "--rm") || strings.Contains(joined, " down ") {
		t.Fatal("helper must survive for result inspection and never down stack")
	}
	f.containers[0].Config.Labels[RoleLabel] = "unrelated"
	if err = f.e.Update(context.Background(), a, io.Discard); err == nil {
		t.Fatal("ordinary container accepted for management update")
	}
}

func TestUpdateRecoveryBlocksMutationsUntilHelperCompletes(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	j := &storedJob{Job: Job{ID: "eab47565-129a-432d-a773-65a400ebc0f1", Status: "running"}, Helper: "docklight-update-test"}
	if err = s.save(j); err != nil {
		t.Fatal(err)
	}
	status := "running"
	fail := false
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if fail {
			return errors.New("Docker unavailable")
		}
		if args[0] == "inspect" {
			return json.NewEncoder(out).Encode(map[string]any{"Status": status, "ExitCode": 0})
		}
		if args[0] == "logs" {
			_, err := io.WriteString(out, "updated successfully")
			return err
		}
		return f.run(ctx, dir, out, args...)
	}
	s, err = NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"kind":"container","id":"` + testID + `","operation":"stop"}`
	for _, unavailable := range []bool{false, true} {
		fail = unavailable
		if w := agentRequest(s, "POST", "/v1/jobs", body, testToken); w.Code != 409 {
			t.Fatalf("unresolved update allowed mutation: %d", w.Code)
		}
	}
	fail = false
	status = "exited"
	w := agentRequest(s, "GET", "/v1/jobs/"+j.ID, "", testToken)
	var result Job
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" || !strings.Contains(result.Output, "updated successfully") {
		t.Fatalf("lost helper result: %#v", result)
	}
}

func TestFailedOperationAndMissingJob(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	w := agentRequest(s, "POST", "/v1/jobs", `{"kind":"container","id":"missing","operation":"stop"}`, testToken)
	var j Job
	if err = json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, s, j.ID); got.Status != "failed" || !strings.Contains(got.Output, "no longer exists") {
		t.Fatalf("failed operation: %#v", got)
	}
	if w = agentRequest(s, "GET", "/v1/jobs/not-a-job", "", testToken); w.Code != 404 {
		t.Fatal("missing job returned success")
	}
}

func TestUncertainHelperLaunchKeepsHostLocked(t *testing.T) {
	f := newFixture(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if len(hostname) < 12 {
		t.Skip("short hostname")
	}
	agent := f.containers[0]
	agent.ID = hostname + strings.Repeat("c", 64)
	agent.Config.Labels = map[string]string{RoleLabel: "agent"}
	f.containers[0].Config.Labels[RoleLabel] = "ui"
	f.containers = append(f.containers, agent)
	base := f.e.Run
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		if args[0] == "run" {
			return context.DeadlineExceeded
		}
		if len(args) > 4 && args[0] == "container" && args[1] == "ls" {
			return nil
		}
		return base(ctx, dir, out, args...)
	}
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	w := agentRequest(s, "POST", "/v1/jobs", `{"kind":"container","id":"`+testID+`","operation":"update","confirmed":true}`, testToken)
	var j Job
	if err = json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	j = waitJob(t, s, j.ID)
	if j.Status != "updating" || !strings.Contains(j.Output, "uncertain") {
		t.Fatalf("unsafe failed launch: %#v", j)
	}
	w = agentRequest(s, "POST", "/v1/jobs", `{"kind":"container","id":"`+testID+`","operation":"stop"}`, testToken)
	if w.Code != 409 {
		t.Fatal("uncertain helper allowed another mutation")
	}
}

func TestHelperCleanupRequiresOwnershipAndTerminalState(t *testing.T) {
	for _, tc := range []struct {
		role, target, state string
		ok                  bool
	}{{"updater", testID, "exited", true}, {"updater", testID, "dead", true}, {"updater", testID, "running", false}, {"ui", testID, "exited", false}, {"updater", "other", "exited", false}} {
		t.Run(tc.role+tc.target[:3]+tc.state, func(t *testing.T) {
			f := newFixture(t)
			c := f.containers[0]
			c.Config.Labels[RoleLabel] = tc.role
			c.Config.Labels["io.docklight.update.target"] = tc.target
			c.State.Status = tc.state
			removed := false
			f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
				if args[0] == "inspect" {
					return json.NewEncoder(out).Encode(c)
				}
				if !reflect.DeepEqual(args, []string{"container", "rm", "helper"}) {
					t.Fatalf("unsafe helper cleanup %v", args)
				}
				removed = true
				return nil
			}
			err := f.e.removeFinishedHelper(context.Background(), "helper", testID)
			if (err == nil) != tc.ok || removed != tc.ok {
				t.Fatalf("cleanup err=%v removed=%v", err, removed)
			}
		})
	}
}

func TestJobPersistenceFailureNeverRunsDocker(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(f.e.DataDir); err != nil {
		t.Fatal(err)
	}
	w := agentRequest(s, "POST", "/v1/jobs", `{"kind":"container","id":"`+testID+`","operation":"stop"}`, testToken)
	if w.Code != 500 || len(f.calls) != 0 {
		t.Fatalf("operation ran without durable job: HTTP %d, calls %v", w.Code, f.calls)
	}
}
