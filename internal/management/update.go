package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

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
	if err := ValidateAction(a); err != nil {
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
