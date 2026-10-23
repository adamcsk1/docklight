package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const maxOutput = 256 << 10

type Runner func(context.Context, string, io.Writer, ...string) error

type Engine struct {
	Root         string
	DataDir      string
	Timeout      time.Duration
	BuildTimeout time.Duration
	Run          Runner
}

func Command(ctx context.Context, dir string, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

type limitedBuffer struct {
	data  []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data)+n > b.limit {
		return 0, errors.New("Docker output exceeds limit")
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (e *Engine) capture(ctx context.Context, args ...string) (string, error) {
	b := &limitedBuffer{limit: 16 << 20}
	err := e.Run(ctx, "", b, args...)
	if err != nil {
		return "", fmt.Errorf("docker %s failed: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return string(b.data), nil
}

type inspectContainer struct {
	ID, Name, Image string
	Config          struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Status string
		Health *struct{ Status string }
	}
	Mounts          []struct{ Type, Name string }
	NetworkSettings struct{ Networks map[string]struct{} }
}

func (e *Engine) containers(ctx context.Context) ([]inspectContainer, error) {
	text, err := e.capture(ctx, "container", "ls", "-aq", "--no-trunc")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(text)
	if len(ids) == 0 {
		return nil, nil
	}
	text, err = e.capture(ctx, append([]string{"container", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var result []inspectContainer
	err = json.Unmarshal([]byte(text), &result)
	return result, err
}

func protected(c inspectContainer) bool {
	l := c.Config.Labels
	role := l[RoleLabel]
	service := l[ServiceLabel]
	name := strings.TrimPrefix(c.Name, "/")
	hostname, _ := os.Hostname()
	return role == "ui" || role == "agent" || role == "updater" || name == "docklight" || name == "docklight-agent" || service == "docklight" || service == "docklight-agent" || (len(hostname) >= 12 && strings.HasPrefix(c.ID, hostname))
}

func (e *Engine) Inventory(ctx context.Context) (Inventory, error) {
	containers, err := e.containers(ctx)
	if err != nil {
		return Inventory{}, err
	}
	inv, err := e.inventory(ctx, containers)
	if err != nil {
		return inv, err
	}
	// Capability discovery must not consume the Docker snapshot's deadline.
	discoveryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e.enrichCompose(discoveryCtx, containers, &inv)
	sort.Slice(inv.Containers, func(i, j int) bool {
		a, b := inv.Containers[i], inv.Containers[j]
		return a.Project+"/"+a.Name < b.Project+"/"+b.Name
	})
	return inv, nil
}

func (e *Engine) enrichCompose(ctx context.Context, containers []inspectContainer, inv *Inventory) {
	type discovery struct {
		services map[string]composeService
		err      error
	}
	projects := map[string]discovery{}
	for i, c := range containers {
		view := &inv.Containers[i]
		t, err := e.composeTarget(c)
		view.ComposeAvailable = err == nil
		if err != nil {
			continue
		}
		key, _ := json.Marshal(t)
		d, ok := projects[string(key)]
		if !ok {
			if ctx.Err() != nil {
				d.err = errors.New("Compose discovery timed out; reconnect to retry")
			} else {
				d.services, d.err = e.composeServices(ctx, t)
			}
			projects[string(key)] = d
		}
		if d.err != nil {
			view.BuildError = d.err.Error()
		} else if service, exists := d.services[c.Config.Labels[ServiceLabel]]; !exists {
			view.BuildError = "Service missing from current Compose configuration"
		} else {
			view.Buildable = service.Build != nil
		}
	}
}

func (e *Engine) inventory(ctx context.Context, containers []inspectContainer) (Inventory, error) {
	var inv Inventory
	images, volumes, networks := map[string][]string{}, map[string][]string{}, map[string][]string{}
	for _, c := range containers {
		name := strings.TrimPrefix(c.Name, "/")
		inv.Containers = append(inv.Containers, Container{ID: c.ID, Name: name, Image: c.Config.Image, ImageID: c.Image, State: c.State.Status, Project: c.Config.Labels[ProjectLabel], Service: c.Config.Labels[ServiceLabel], Protected: protected(c)})
		images[c.Image] = append(images[c.Image], name)
		for _, m := range c.Mounts {
			if m.Type == "volume" {
				volumes[m.Name] = append(volumes[m.Name], name)
			}
		}
		for n := range c.NetworkSettings.Networks {
			networks[n] = append(networks[n], name)
		}
	}
	for _, kind := range []string{"image", "volume", "network"} {
		args := []string{kind, "ls", "-q"}
		if kind != "volume" {
			args = append(args, "--no-trunc")
		}
		text, err := e.capture(ctx, args...)
		if err != nil {
			return inv, err
		}
		ids := strings.Fields(text)
		seen := map[string]bool{}
		var unique []string
		for _, id := range ids {
			if !seen[id] {
				unique = append(unique, id)
				seen[id] = true
			}
		}
		if len(unique) == 0 {
			continue
		}
		text, err = e.capture(ctx, append([]string{kind, "inspect"}, unique...)...)
		if err != nil {
			return inv, err
		}
		var items []struct {
			ID, Name string
			RepoTags []string
		}
		if err = json.Unmarshal([]byte(text), &items); err != nil {
			return inv, err
		}
		for _, item := range items {
			r := Resource{ID: item.ID, Name: item.Name, Kind: kind}
			switch kind {
			case "image":
				r.Name = strings.Join(item.RepoTags, ", ")
				if r.Name == "" {
					r.Name = item.ID
				}
				r.UsedBy = images[item.ID]
			case "volume":
				r.ID = item.Name
				r.UsedBy = volumes[item.Name]
			case "network":
				r.UsedBy = networks[item.Name]
				r.Protected = item.Name == "bridge" || item.Name == "host" || item.Name == "none"
			}
			inv.Resources = append(inv.Resources, r)
		}
	}
	sort.Slice(inv.Resources, func(i, j int) bool {
		a, b := inv.Resources[i], inv.Resources[j]
		return a.Kind+"/"+a.Name < b.Kind+"/"+b.Name
	})
	return inv, nil
}
