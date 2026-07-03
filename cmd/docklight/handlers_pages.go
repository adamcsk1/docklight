package main

import (
	"net/http"
	"strconv"
	"strings"
)

func (app *App) dashboard(w http.ResponseWriter, r *http.Request) {
	stacks, err := app.loadStacks(true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.render(w, r, "dashboard.html", PageData{Title: "Dashboard", Stacks: stacks, Config: app.cfg, UsageScript: true})
}

func (app *App) stackDetail(w http.ResponseWriter, r *http.Request) {
	stack, err := app.loadStack(r.PathValue("name"), false)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	operation, ok, err := app.stackPageOperation(r, stack.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := PageData{Title: stack.Name, Stack: stack, UsageScript: true}
	if ok {
		data.StackOperation = &operation
		data.StackOperationScript = true
	} else {
		app.enrichLive(&stack)
		data.Stack = stack
	}
	app.render(w, r, "stack.html", data)
}

func (app *App) stackPageOperation(r *http.Request, stackName string) (Activity, bool, error) {
	if rawID := r.URL.Query().Get("operation"); rawID != "" {
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || id <= 0 {
			return Activity{}, false, nil
		}
		activity, err := app.loadActivityByID(id)
		if err != nil || activity.StackName != stackName {
			return Activity{}, false, nil
		}
		return activity, true, nil
	}
	return app.loadLatestPendingActivity(stackName)
}

func (app *App) newStackPage(w http.ResponseWriter, r *http.Request) {
	app.render(w, r, "stack_new.html", PageData{Title: "New stack"})
}

func (app *App) activity(w http.ResponseWriter, r *http.Request) {
	activities, err := app.loadActivity()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.render(w, r, "activity.html", PageData{Title: "Activity", Activities: activities, ClipboardScript: true})
}

func (app *App) settings(w http.ResponseWriter, r *http.Request) {
	settings, err := app.loadSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	verboseLogFiles, err := app.verboseLogFiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.render(w, r, "settings.html", PageData{Title: "Settings", Config: app.cfg, Settings: settings, RetentionDayOptions: retentionDayOptions, VerboseLogFiles: verboseLogFiles, BuildDate: buildDate})
}

func (app *App) containers(w http.ResponseWriter, r *http.Request) {
	containers, err := app.dockerContainerResources(true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.render(w, r, "containers.html", PageData{Title: "Containers", Containers: containers})
}

func (app *App) standaloneExternalContainersPage(w http.ResponseWriter, r *http.Request) {
	containers, err := app.dockerContainerResources(true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filtered := make([]DockerContainer, 0, len(containers))
	for _, container := range containers {
		if container.Stack == "standalone" || strings.HasPrefix(container.Stack, "external:") {
			filtered = append(filtered, container)
		}
	}
	app.render(w, r, "external_containers.html", PageData{Title: "Standalone / external containers", Containers: filtered, UsageScript: true})
}

func (app *App) images(w http.ResponseWriter, r *http.Request) {
	images, err := app.dockerImages()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.render(w, r, "images.html", PageData{Title: "Images", Images: images})
}

func (app *App) volumes(w http.ResponseWriter, r *http.Request) {
	volumes, err := app.dockerVolumes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.render(w, r, "volumes.html", PageData{Title: "Volumes", Volumes: volumes})
}

func (app *App) networks(w http.ResponseWriter, r *http.Request) {
	networks, err := app.dockerNetworks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.render(w, r, "networks.html", PageData{Title: "Networks", Networks: networks})
}
