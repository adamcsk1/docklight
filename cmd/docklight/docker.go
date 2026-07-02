package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

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

func (app *App) dockerContainerLogs(container string) (string, error) {
	return app.runDocker("logs", "--tail", "300", container)
}

func (app *App) dockerContainerExec(container, command string) (string, error) {
	return app.runDocker("exec", container, "sh", "-lc", command)
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
	output, err := os.CreateTemp("", "docklight-command-*.log")
	if err != nil {
		return "", err
	}
	defer os.Remove(output.Name())
	defer output.Close()
	cmd.Stdout = output
	cmd.Stderr = output
	err = cmd.Run()
	if _, seekErr := output.Seek(0, 0); seekErr != nil {
		return "", seekErr
	}
	outputBytes, readErr := io.ReadAll(output)
	if readErr != nil {
		return "", readErr
	}
	outputText := string(outputBytes)
	app.verboseLog("command completed", "dir", dir, "command", name+" "+strings.Join(args, " "), "status", commandStatus(err), "duration", time.Since(start).String(), "output_bytes", fmt.Sprintf("%d", len(outputText)))
	return outputText, err
}

func commandStatus(err error) string {
	if err != nil {
		return "failed"
	}
	return "success"
}
