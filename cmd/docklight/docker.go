package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxCommandOutput = 256 * 1024

type dockerContainerInspect struct {
	ID     string
	Name   string
	Image  string
	Config struct {
		Image  string
		Env    []string
		Labels map[string]string
	}
	State struct {
		Status    string
		StartedAt string
		Health    *struct{ Status string }
	}
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIP, HostPort string }
		Networks map[string]struct{}
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
	}
}

type dockerImageInspect struct {
	ID       string
	RepoTags []string
	Size     int64
	Created  string
}

type dockerVolumeInspect struct {
	Name       string
	Driver     string
	Mountpoint string
	CreatedAt  string
}

type dockerNetworkInspect struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Internal   bool
	Attachable bool
}

func (app *App) dockerContainerInspects(all bool) ([]dockerContainerInspect, error) {
	args := []string{"container", "ls", "-q"}
	if all {
		args = []string{"container", "ls", "-aq"}
	}
	ids, err := app.dockerIDs(args...)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	output, err := app.runDocker(append([]string{"container", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var inspected []dockerContainerInspect
	return inspected, json.Unmarshal([]byte(output), &inspected)
}

func (app *App) dockerIDs(args ...string) ([]string, error) {
	output, err := app.runDocker(args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids, nil
}

func (app *App) runDocker(args ...string) (string, error) {
	return app.runCommand("", "docker", args...)
}

func (app *App) runCommand(dir, name string, args ...string) (string, error) {
	start := time.Now()
	if app.runner != nil {
		output, err := app.runner(dir, name, args...)
		app.verboseLog("command completed", "dir", dir, "command", name+" "+strings.Join(args, " "), "status", commandStatus(err), "duration", time.Since(start).String(), "output_bytes", fmt.Sprintf("%d", len(output)))
		return output, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), app.cfg.ActionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output := &limitedOutput{limit: maxCommandOutput}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	outputText := output.String()
	app.verboseLog("command completed", "dir", dir, "command", name+" "+strings.Join(args, " "), "status", commandStatus(err), "duration", time.Since(start).String(), "output_bytes", fmt.Sprintf("%d", len(outputText)))
	return outputText, err
}

func commandStatus(err error) string {
	if err != nil {
		return "failed"
	}
	return "success"
}

type limitedOutput struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = w.buf.Write(p[:remaining])
			w.truncated = true
		} else {
			_, _ = w.buf.Write(p)
		}
	} else {
		w.truncated = true
	}
	return len(p), nil
}

func (w *limitedOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.buf.String()
	if w.truncated {
		out += "\n... output truncated"
	}
	return out
}
