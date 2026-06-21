package main

import (
	"fmt"
	"net/http"
	"strings"
)

func (app *App) deleteSelectedVolumes(w http.ResponseWriter, r *http.Request) {
	force := r.FormValue("force") == "on"
	selected := r.Form["ids"]
	if len(selected) == 0 {
		app.redirectWithError(w, r, "/volumes", "no volumes selected")
		return
	}

	var names []string
	for _, name := range selected {
		volume, ok, err := app.findVolume(name)
		if err != nil {
			app.redirectWithError(w, r, "/volumes", err.Error())
			return
		}
		if !ok {
			app.redirectWithError(w, r, "/volumes", "volume not found: "+name)
			return
		}
		if volume.Protected {
			app.redirectWithError(w, r, "/volumes", "Docklight cannot delete its own volume: "+volume.Name)
			return
		}
		if !volume.Unused {
			app.redirectWithError(w, r, "/volumes", "volume is in use: "+volume.Name)
			return
		}
		names = append(names, volume.Name)
	}

	args := []string{"volume", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, names...)
	output, err := app.runDocker(args...)
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity("docker", "volume delete selected", status, strings.Join(names, "\n")+"\n"+output)
	if err != nil {
		app.redirectWithError(w, r, "/volumes", output)
		return
	}
	app.redirectWithMessage(w, r, "/volumes", fmt.Sprintf("deleted %d selected volumes", len(names)))
}
