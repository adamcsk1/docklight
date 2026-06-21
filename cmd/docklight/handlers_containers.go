package main

import (
	"fmt"
	"net/http"
	"strings"
)

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
