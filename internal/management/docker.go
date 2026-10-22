package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxOutput = 256 << 10

type Runner func(context.Context, string, io.Writer, ...string) error

type Engine struct {
	Root    string
	DataDir string
	Timeout time.Duration
	Run     Runner
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
	State           struct{ Status string }
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
	var inv Inventory
	containers, err := e.containers(ctx)
	if err != nil {
		return inv, err
	}
	images, volumes, networks := map[string][]string{}, map[string][]string{}, map[string][]string{}
	for _, c := range containers {
		_, composeErr := e.composeTarget(c)
		name := strings.TrimPrefix(c.Name, "/")
		inv.Containers = append(inv.Containers, Container{ID: c.ID, Name: name, Image: c.Config.Image, ImageID: c.Image, State: c.State.Status, Project: c.Config.Labels[ProjectLabel], Service: c.Config.Labels[ServiceLabel], Protected: protected(c), ComposeAvailable: composeErr == nil})
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
	sort.Slice(inv.Containers, func(i, j int) bool {
		a, b := inv.Containers[i], inv.Containers[j]
		return a.Project+"/"+a.Name < b.Project+"/"+b.Name
	})
	sort.Slice(inv.Resources, func(i, j int) bool {
		a, b := inv.Resources[i], inv.Resources[j]
		return a.Kind+"/"+a.Name < b.Kind+"/"+b.Name
	})
	return inv, nil
}

type composeTarget struct {
	Dir, Project string
	Files        []string
}

func (e *Engine) safePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("Compose paths must be absolute")
	}
	root, err := filepath.EvalSymlinks(e.Root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("Compose path outside configured root")
	}
	return real, nil
}

func (e *Engine) composeTarget(c inspectContainer) (composeTarget, error) {
	l := c.Config.Labels
	t := composeTarget{Project: l[ProjectLabel]}
	if t.Project == "" || l[ServiceLabel] == "" || strings.HasPrefix(t.Project, "-") || strings.HasPrefix(l[ServiceLabel], "-") {
		return t, errors.New("Compose labels missing or invalid")
	}
	var err error
	t.Dir, err = e.safePath(l[WorkingDirLabel])
	if err != nil {
		return t, err
	}
	info, err := os.Stat(t.Dir)
	if err != nil || !info.IsDir() {
		return t, errors.New("Compose directory unavailable")
	}
	for _, p := range strings.Split(l[FilesLabel], ",") {
		// Compose records our temporary image override in config_files after recreation.
		// Never treat that implementation detail as user Compose configuration.
		clean := filepath.Clean(strings.TrimSpace(p))
		if filepath.Dir(clean) == filepath.Clean(e.DataDir) && strings.HasPrefix(filepath.Base(clean), "recreate-") && strings.HasSuffix(clean, ".json") {
			continue
		}
		file, err := e.safePath(strings.TrimSpace(p))
		if err != nil {
			return t, err
		}
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() {
			return t, errors.New("Compose file unavailable")
		}
		t.Files = append(t.Files, file)
	}
	if len(t.Files) == 0 {
		return t, errors.New("original Compose files missing")
	}
	return t, nil
}

func (t composeTarget) args() []string {
	args := []string{"compose", "--project-name", t.Project, "--project-directory", t.Dir}
	for _, f := range t.Files {
		args = append(args, "-f", f)
	}
	return args
}
