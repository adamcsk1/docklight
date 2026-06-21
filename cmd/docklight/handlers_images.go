package main

import (
	"fmt"
	"net/http"
	"strings"
)

func (app *App) deleteSelectedImages(w http.ResponseWriter, r *http.Request) {
	force := r.FormValue("force") == "on"
	selected := r.Form["ids"]
	if len(selected) == 0 {
		app.redirectWithError(w, r, "/images", "no images selected")
		return
	}

	var ids []string
	var names []string
	for _, id := range selected {
		image, ok, err := app.findImage(id)
		if err != nil {
			app.redirectWithError(w, r, "/images", err.Error())
			return
		}
		if !ok {
			app.redirectWithError(w, r, "/images", "image not found: "+id)
			return
		}
		if image.Protected {
			app.redirectWithError(w, r, "/images", "Docklight cannot delete its own image: "+image.Name)
			return
		}
		if !image.Unused && !force {
			app.redirectWithError(w, r, "/images", "image is in use; enable force to ask Docker to remove it anyway: "+image.Name)
			return
		}
		ids = append(ids, image.ShortID)
		names = append(names, image.Name)
	}

	args := []string{"image", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, ids...)
	output, err := app.runDocker(args...)
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity("docker", "image delete selected", status, strings.Join(names, "\n")+"\n"+output)
	if err != nil {
		app.redirectWithError(w, r, "/images", output)
		return
	}
	app.redirectWithMessage(w, r, "/images", fmt.Sprintf("deleted %d selected images", len(ids)))
}
