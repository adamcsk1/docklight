package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
)

// ValidateAction checks the fixed API's action shape, not current Docker state.
func ValidateAction(a Action) error {
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
	if err := ValidateAction(a); err != nil {
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
	containers, err := e.containers(ctx)
	if err != nil {
		return err
	}
	inv, err := e.inventory(ctx, containers)
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
