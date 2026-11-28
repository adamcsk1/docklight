package main

import (
	"context"
	"net/http"
	"slices"
	"time"
)

type imageCheckHost struct {
	Name, Detail                           string
	Current, Available, Unknown, Untracked int
}

type imageCheckRun struct {
	Status, Message string
	Selected        []string
	Hosts           []imageCheckHost
}

func (app *App) imageCheckStart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	master := app.masterHost()
	if master == nil || r.PostForm.Get("host") != master.ID {
		http.Error(w, "master host required", 400)
		return
	}
	if app.imageMonitor == nil {
		http.Error(w, "image update checks are disabled", 409)
		return
	}
	selected := r.PostForm["selected"]
	if len(selected) == 0 {
		http.Error(w, "select at least one host", 400)
		return
	}
	for i, id := range selected {
		if _, err := app.host(id); err != nil || id == "" || slices.Contains(selected[:i], id) {
			http.Error(w, "unknown or duplicate selected host", 400)
			return
		}
	}
	if !app.allowManagement(w, r) {
		return
	}
	if !app.imageUpdatesAllowed() {
		http.Error(w, "master ownership is not verified; wait for host verification before checking images", 409)
		return
	}
	app.imageCheckMu.Lock()
	defer app.imageCheckMu.Unlock()
	if app.imageCheck.Status == "queued" || app.imageCheck.Status == "checking" {
		http.Error(w, "an image update check is already pending", 409)
		return
	}
	app.imageCheck = imageCheckRun{Status: "queued", Message: "Waiting for the image update worker.", Selected: slices.Clone(selected)}
	select {
	case app.imageCheckWake <- struct{}{}:
	default:
	}
	http.Redirect(w, r, app.withBase("/")+"?host="+master.ID+"&view=maintenance", http.StatusSeeOther)
}

func (app *App) runRequestedImageCheck(parent context.Context) bool {
	app.imageCheckMu.Lock()
	if app.imageCheck.Status != "queued" {
		app.imageCheckMu.Unlock()
		return false
	}
	run := app.imageCheck
	run.Status, run.Message = "checking", "Checking selected hosts; no images will be pulled or deployed."
	app.imageCheck = run
	app.imageCheckMu.Unlock()

	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	err := app.imageMonitor.CheckSelected(ctx, time.Now().UTC(), run.Selected)
	cancel()
	run.Status, run.Message = "complete", "Selected hosts checked. Inventory badges use these results."
	if err != nil {
		run.Status, run.Message = "incomplete", "Check interrupted or results could not be saved. Inspect the results before retrying."
	}
	for _, h := range app.cfg.Hosts {
		if !slices.Contains(run.Selected, h.ID) {
			continue
		}
		state := app.imageMonitor.Host(h.ID, h.URL)
		host := imageCheckHost{Name: h.Name, Detail: state.Error}
		for _, result := range state.Results {
			switch result.Status {
			case "current":
				host.Current++
			case "available":
				host.Available++
			case "untracked":
				host.Untracked++
			default:
				host.Unknown++
			}
		}
		run.Hosts = append(run.Hosts, host)
	}
	app.imageCheckMu.Lock()
	app.imageCheck = run
	app.imageCheckMu.Unlock()
	return true
}
