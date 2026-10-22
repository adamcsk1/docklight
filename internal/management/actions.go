package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

func validateAction(a Action) error {
	if a.ID == "" || len(a.ID) > 256 || strings.ContainsAny(a.ID, "\x00\r\n") {
		return errors.New("invalid target")
	}
	if a.Scope != "" && a.Scope != "service" && a.Scope != "stack" {
		return errors.New("invalid scope")
	}
	switch a.Kind {
	case "container":
		switch a.Operation {
		case "start", "stop", "delete", "recreate", "upgrade", "build", "update":
		default:
			return errors.New("unknown operation")
		}
	case "image", "volume", "network":
		if a.Operation != "delete" || a.Scope != "" {
			return errors.New("invalid resource operation")
		}
	default:
		return errors.New("unknown resource kind")
	}
	if (a.Operation == "delete" || a.Operation == "update") && !a.Confirmed {
		return errors.New("explicit confirmation required")
	}
	return nil
}

func (e *Engine) Execute(ctx context.Context, a Action, out io.Writer) error {
	if err := validateAction(a); err != nil {
		return err
	}
	if a.Kind != "container" {
		return e.deleteResource(ctx, a, out)
	}
	containers, err := e.containers(ctx)
	if err != nil {
		return err
	}
	var target *inspectContainer
	for i := range containers {
		if containers[i].ID == a.ID {
			target = &containers[i]
			break
		}
	}
	if target == nil {
		return errors.New("container no longer exists")
	}
	if a.Operation == "update" {
		return errors.New("use dedicated update helper")
	}
	var selected []inspectContainer
	for _, c := range containers {
		match := c.ID == target.ID
		if a.Scope != "" {
			if target.Config.Labels[ProjectLabel] == "" {
				return errors.New("standalone containers have no Compose scope")
			}
			match = c.Config.Labels[ProjectLabel] == target.Config.Labels[ProjectLabel]
			if a.Scope == "service" {
				match = match && c.Config.Labels[ServiceLabel] == target.Config.Labels[ServiceLabel]
			}
		}
		if match {
			if protected(c) {
				return errors.New("management containers require dedicated update action")
			}
			if a.Operation == "delete" && c.State.Status != "exited" && c.State.Status != "created" && c.State.Status != "dead" {
				return errors.New("stop containers before deletion")
			}
			selected = append(selected, c)
		}
	}
	switch a.Operation {
	case "start", "stop", "delete":
		op := a.Operation
		if op == "delete" {
			op = "rm"
		}
		args := []string{"container", op}
		for _, c := range selected {
			args = append(args, c.ID)
		}
		return e.Run(ctx, "", out, args...)
	case "recreate", "upgrade", "build":
		// Compose recreation always affects every replica of the selected service.
		if a.Scope == "" {
			return errors.New("Compose recreation requires service or stack scope")
		}
		t, err := e.composeTarget(*target)
		if err != nil {
			return err
		}
		images := map[string]string{}
		for _, c := range selected {
			other, err := e.composeTarget(c)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(t, other) {
				return errors.New("inconsistent Compose configuration within project")
			}
			service := c.Config.Labels[ServiceLabel]
			if previous := images[service]; a.Operation == "recreate" && previous != "" && previous != c.Image {
				return errors.New("replicas use different images; cannot preserve exact image")
			}
			images[service] = c.Image
		}
		var services []string
		for service := range images {
			services = append(services, service)
		}
		sort.Strings(services)
		args := t.args()
		if a.Operation == "upgrade" || a.Operation == "build" {
			config, err := e.composeServices(ctx, t)
			if err != nil {
				return err
			}
			var filtered []string
			for _, service := range services {
				definition, ok := config[service]
				if !ok {
					return fmt.Errorf("service %s missing from Compose configuration", service)
				}
				if (definition.Build != nil) == (a.Operation == "build") {
					filtered = append(filtered, service)
				}
			}
			services = filtered
			if len(services) == 0 {
				return errors.New("no matching services: use Build for local images or Upgrade for registry images")
			}
			step := []string{"pull", "--ignore-buildable", "--policy", "always"}
			if a.Operation == "build" {
				step = []string{"build"}
			}
			fmt.Fprintf(out, "%s selected Compose services: %s\n", a.Operation, strings.Join(services, ", "))
			if err = e.Run(ctx, t.Dir, out, append(append([]string{}, args...), append(step, services...)...)...); err != nil {
				return err
			}
		} else {
			// Override mutable tags with the exact image IDs currently in use.
			override := struct {
				Services map[string]map[string]string `json:"services"`
			}{Services: map[string]map[string]string{}}
			for s, id := range images {
				override.Services[s] = map[string]string{"image": id, "pull_policy": "never"}
			}
			b, err := json.Marshal(override)
			if err != nil {
				return err
			}
			f, err := os.CreateTemp(e.DataDir, "recreate-*.json")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			if _, err = f.Write(b); err != nil {
				f.Close()
				return err
			}
			if err = f.Close(); err != nil {
				return err
			}
			args = append(args, "-f", f.Name())
		}
		up := []string{"up", "-d", "--no-deps", "--no-build", "--pull", "never"}
		if a.Operation == "recreate" {
			up = append(up, "--force-recreate")
		}
		up = append(up, services...)
		return e.Run(ctx, t.Dir, out, append(args, up...)...)
	}
	return errors.New("unsupported action")
}

func (e *Engine) deleteResource(ctx context.Context, a Action, out io.Writer) error {
	// Include stopped-container references and recheck immediately before removal.
	inv, err := e.Inventory(ctx)
	if err != nil {
		return err
	}
	for _, r := range inv.Resources {
		if r.Kind == a.Kind && r.ID == a.ID {
			if r.Protected || len(r.UsedBy) > 0 {
				return errors.New("resource is in use or protected")
			}
			return e.Run(ctx, "", out, r.Kind, "rm", r.ID)
		}
	}
	return errors.New("resource no longer exists")
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func shellCommand(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func HelperName(a Action) string {
	return "docklight-update-" + a.ID[:min(12, len(a.ID))]
}

// A failed launch response can still mean the detached helper started.
type helperLaunchError struct{ error }

func (e *Engine) Update(ctx context.Context, a Action, out io.Writer) error {
	if err := validateAction(a); err != nil {
		return err
	}
	if a.Kind != "container" || a.Operation != "update" || a.Scope != "" {
		return errors.New("invalid update action")
	}
	containers, err := e.containers(ctx)
	if err != nil {
		return err
	}
	var target, agent *inspectContainer
	hostname, _ := os.Hostname()
	for i := range containers {
		c := &containers[i]
		if c.ID == a.ID {
			target = c
		}
		if c.Config.Labels[RoleLabel] == "agent" && len(hostname) >= 12 && strings.HasPrefix(c.ID, hostname) {
			agent = c
		}
	}
	if target == nil || agent == nil {
		return errors.New("update requires identifiable agent and target containers")
	}
	role := target.Config.Labels[RoleLabel]
	if role != "ui" && role != "agent" {
		return errors.New("dedicated update is only for labeled UI/agent containers")
	}
	t, err := e.composeTarget(*target)
	if err != nil {
		return err
	}
	service := target.Config.Labels[ServiceLabel]
	base := t.args()
	pull := append(append([]string{"docker"}, base...), "pull", "--policy", "always", service)
	up := append(append([]string{"docker"}, base...), "up", "-d", "--no-deps", "--no-build", "--pull", "never", service)
	script := "set -eu\n" + shellCommand(pull) + "\n" + shellCommand(up)
	// Mounts include socket, read-only Compose root and optional registry credentials.
	name := HelperName(a)
	old, err := e.capture(ctx, "container", "ls", "-aq", "--filter", "name=^/"+name+"$")
	if err != nil {
		return err
	}
	if strings.TrimSpace(old) != "" {
		labelsJSON, err := e.capture(ctx, "inspect", "--format", "{{json .Config.Labels}}", name)
		if err != nil {
			return err
		}
		var labels map[string]string
		if err = json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			return err
		}
		if labels[RoleLabel] != "updater" || labels["io.docklight.update.target"] != a.ID {
			return errors.New("update helper name belongs to another container")
		}
		state, err := e.capture(ctx, "inspect", "--format", "{{.State.Status}}", name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(state) != "exited" {
			return errors.New("update helper already exists and is not finished")
		}
		if err = e.Run(ctx, "", out, "container", "rm", name); err != nil {
			return err
		}
	}
	err = e.Run(ctx, "", out, "run", "-d", "--name", name, "--label", RoleLabel+"=updater", "--label", "io.docklight.update.target="+a.ID, "--volumes-from", agent.ID+":ro", "--workdir", filepath.Clean(t.Dir), "--entrypoint", "/bin/sh", agent.Image, "-c", script)
	if err != nil {
		return helperLaunchError{err}
	}
	return nil
}

func (e *Engine) helperResult(ctx context.Context, name string) (string, string, error) {
	text, err := e.capture(ctx, "inspect", "--format", "{{json .State}}", name)
	if err != nil {
		return "failed", "Update helper unavailable; inspect host before retrying", err
	}
	var state struct {
		Status   string
		ExitCode int
	}
	if err = json.Unmarshal([]byte(text), &state); err != nil {
		return "failed", "Invalid helper state", err
	}
	if state.Status != "exited" && state.Status != "dead" {
		return "updating", "Update helper running", nil
	}
	text, err = e.capture(ctx, "logs", "--tail", "200", name)
	if err != nil {
		return "failed", "Update helper logs unavailable", err
	}
	status := "success"
	if state.ExitCode != 0 || state.Status == "dead" {
		status = "failed"
	}
	if len(text) > maxOutput {
		text = "[earlier output truncated]\n" + text[len(text)-maxOutput:]
	}
	return status, fmt.Sprintf("Helper exit code: %d\n%s", state.ExitCode, text), nil
}

func (e *Engine) removeFinishedHelper(ctx context.Context, name, target string) error {
	text, err := e.capture(ctx, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return err
	}
	var c inspectContainer
	if err = json.Unmarshal([]byte(text), &c); err != nil {
		return err
	}
	if c.Config.Labels[RoleLabel] != "updater" || c.Config.Labels["io.docklight.update.target"] != target || (c.State.Status != "exited" && c.State.Status != "dead") {
		return errors.New("helper is not an owned, finished updater")
	}
	return e.Run(ctx, "", io.Discard, "container", "rm", name)
}
