package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type composeTarget struct {
	Dir, Project string
	Files        []string
	EnvFiles     []string
}

type composeService struct {
	Build json.RawMessage `json:"build"`
}

func (e *Engine) composeServices(ctx context.Context, t composeTarget) (map[string]composeService, error) {
	// Resolved configuration may contain secrets; never send it or parser warnings to job logs.
	f, err := os.CreateTemp(e.DataDir, ".compose-*.json")
	if err != nil {
		return nil, errors.New("cannot create private Compose discovery file")
	}
	defer os.Remove(f.Name())
	if err = f.Close(); err != nil {
		return nil, err
	}
	args := append(t.args(), "config", "--format", "json", "--no-env-resolution", "--output", f.Name())
	if err = e.Run(ctx, t.Dir, io.Discard, args...); err != nil {
		return nil, errors.New("Compose config unavailable; check mounted files, .env and required variables on the agent")
	}
	f, err = os.Open(f.Name())
	if err != nil {
		return nil, errors.New("cannot read Compose configuration")
	}
	defer f.Close()
	var config struct {
		Services map[string]composeService `json:"services"`
	}
	if err = json.NewDecoder(io.LimitReader(f, 16<<20)).Decode(&config); err != nil || len(config.Services) == 0 {
		return nil, errors.New("invalid or oversized Compose configuration")
	}
	for name, service := range config.Services {
		if string(service.Build) == "null" {
			service.Build = nil
			config.Services[name] = service
		}
	}
	return config.Services, nil
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
	if value := l[EnvironmentFileLabel]; value != "" {
		for _, p := range strings.Split(value, ",") {
			file, err := e.safePath(strings.TrimSpace(p))
			if err != nil {
				return t, err
			}
			info, err := os.Stat(file)
			if err != nil || !info.Mode().IsRegular() {
				return t, errors.New("Compose environment file unavailable")
			}
			t.EnvFiles = append(t.EnvFiles, file)
		}
	}
	return t, nil
}

func (t composeTarget) args() []string {
	args := []string{"compose", "--project-name", t.Project, "--project-directory", t.Dir}
	for _, f := range t.Files {
		args = append(args, "-f", f)
	}
	for _, f := range t.EnvFiles {
		args = append(args, "--env-file", f)
	}
	return args
}
