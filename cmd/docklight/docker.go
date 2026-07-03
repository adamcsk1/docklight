package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	return app.runCommandProgress(dir, name, nil, args...)
}

func (app *App) runCommandProgress(dir, name string, progress operationProgress, args ...string) (string, error) {
	start := time.Now()
	if app.runner != nil {
		output, err := app.runner(dir, name, args...)
		if progress != nil {
			progress(output)
		}
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
	writer := &commandProgressWriter{file: output, progress: progress}
	stopProgress := writer.startProgress()
	cmd.Stdout = writer
	cmd.Stderr = writer
	err = cmd.Run()
	if stopProgress != nil {
		stopProgress()
	}
	outputText, readErr := writer.String()
	if readErr != nil {
		return "", readErr
	}
	app.verboseLog("command completed", "dir", dir, "command", name+" "+strings.Join(args, " "), "status", commandStatus(err), "duration", time.Since(start).String(), "output_bytes", fmt.Sprintf("%d", len(outputText)))
	return outputText, err
}

type commandProgressWriter struct {
	mu       sync.Mutex
	file     *os.File
	progress operationProgress
}

func (w *commandProgressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Write(p)
}

func (w *commandProgressWriter) startProgress() func() {
	if w.progress == nil {
		return nil
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		lastOutput := ""
		offset := int64(0)
		ticker := time.NewTicker(operationProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				chunk, nextOffset, err := w.readSince(offset)
				if err == nil && chunk != "" {
					offset = nextOffset
					lastOutput += chunk
					w.progress(lastOutput)
				}
			case <-stop:
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (w *commandProgressWriter) String() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stringLocked()
}

func (w *commandProgressWriter) readSince(offset int64) (string, int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.file.Seek(offset, 0); err != nil {
		return "", offset, err
	}
	outputBytes, err := io.ReadAll(w.file)
	if err != nil {
		return "", offset, err
	}
	nextOffset, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", offset, err
	}
	if _, err := w.file.Seek(0, io.SeekEnd); err != nil {
		return "", offset, err
	}
	return string(outputBytes), nextOffset, nil
}

func (w *commandProgressWriter) stringLocked() (string, error) {
	if _, err := w.file.Seek(0, 0); err != nil {
		return "", err
	}
	outputBytes, err := io.ReadAll(w.file)
	if err != nil {
		return "", err
	}
	if _, err := w.file.Seek(0, io.SeekEnd); err != nil {
		return "", err
	}
	return string(outputBytes), nil
}
func commandStatus(err error) string {
	if err != nil {
		return "failed"
	}
	return "success"
}
