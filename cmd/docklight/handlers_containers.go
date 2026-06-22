package main

import (
	"fmt"
	"net/http"
	"strings"
)

func (app *App) containerLogsPage(w http.ResponseWriter, r *http.Request) {
	container, logs, ok := app.containerLogs(w, r)
	if !ok {
		return
	}
	app.render(w, r, "container_logs.html", PageData{Title: container.Name + " logs", Container: container, ContainerLog: logs, ContainerLogScript: true})
}

func (app *App) containerLogsRaw(w http.ResponseWriter, r *http.Request) {
	_, logs, ok := app.containerLogs(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
}

func (app *App) containerExecPage(w http.ResponseWriter, r *http.Request) {
	container, found, err := app.findDockerContainer(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	app.render(w, r, "container_exec.html", PageData{Title: container.Name + " command", Container: container})
}

func (app *App) containerExec(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		app.redirectWithError(w, r, "/containers", "bad form")
		return
	}
	command := strings.TrimSpace(r.FormValue("command"))
	container, found, err := app.findDockerContainer(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	data := PageData{Title: container.Name + " command", Container: container, ContainerCommand: command}
	if command == "" {
		data.Error = "command is required"
		app.render(w, r, "container_exec.html", data)
		return
	}
	if container.Protected {
		data.Error = "Docklight cannot run commands against its own container: " + container.Name
		app.render(w, r, "container_exec.html", data)
		return
	}
	if !strings.EqualFold(container.State, "running") {
		data.Error = "container is not running"
		app.render(w, r, "container_exec.html", data)
		return
	}

	output, err := app.dockerContainerExec(container.ID, command)
	status := "success"
	if err != nil {
		status = "failed"
		data.ContainerExecFailed = true
	}
	app.logActivity(container.Stack, "container exec", status, fmt.Sprintf("%s\n$ %s\noutput bytes: %d", container.Name, command, len(output)))
	data.ContainerExecOutput = output
	if output == "" && err == nil {
		data.ContainerExecOutput = "command completed with no output"
	}
	if err != nil && strings.TrimSpace(output) == "" {
		data.ContainerExecOutput = "command failed with no output"
	}
	app.render(w, r, "container_exec.html", data)
}

func (app *App) containerLogs(w http.ResponseWriter, r *http.Request) (DockerContainer, string, bool) {
	container, found, err := app.findDockerContainer(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return DockerContainer{}, "", false
	}
	if !found {
		http.NotFound(w, r)
		return DockerContainer{}, "", false
	}
	logs, err := app.dockerContainerLogs(container.ID)
	if err != nil {
		message := strings.TrimSpace(logs)
		if message == "" {
			message = "logs unavailable"
		}
		http.Error(w, message, http.StatusInternalServerError)
		return DockerContainer{}, "", false
	}
	return container, logs, true
}

func (app *App) deleteSelectedContainers(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		app.redirectWithError(w, r, "/containers", "bad form")
		return
	}
	force := r.FormValue("force") == "on"
	selected := r.Form["ids"]
	if len(selected) == 0 {
		app.redirectWithError(w, r, "/containers", "no containers selected")
		return
	}

	containers, err := app.dockerContainerResources(true)
	if err != nil {
		app.redirectWithError(w, r, "/containers", err.Error())
		return
	}
	byID := make(map[string]DockerContainer)
	for _, container := range containers {
		byID[container.ID] = container
	}

	var ids []string
	var names []string
	for _, id := range selected {
		container, ok := byID[id]
		if !ok {
			app.redirectWithError(w, r, "/containers", "container not found: "+id)
			return
		}
		if container.Protected {
			app.redirectWithError(w, r, "/containers", "Docklight cannot delete its own container: "+container.Name)
			return
		}
		if strings.EqualFold(container.State, "running") && !force {
			app.redirectWithError(w, r, "/containers", "container is running; enable force to remove it: "+container.Name)
			return
		}
		ids = append(ids, container.ID)
		names = append(names, container.Name)
	}

	args := []string{"container", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, ids...)
	output, err := app.runDocker(args...)
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity("docker", "container delete selected", status, strings.Join(names, "\n")+"\n"+output)
	if err != nil {
		app.redirectWithError(w, r, "/containers", output)
		return
	}
	app.redirectWithMessage(w, r, "/containers", fmt.Sprintf("deleted %d selected containers", len(ids)))
}
