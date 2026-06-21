package main

import (
	"fmt"
	"net/http"
	"strings"
)

func (app *App) deleteSelectedNetworks(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		app.redirectWithError(w, r, "/networks", "bad form")
		return
	}
	selected := r.Form["ids"]
	if len(selected) == 0 {
		app.redirectWithError(w, r, "/networks", "no networks selected")
		return
	}

	var ids []string
	var names []string
	for _, id := range selected {
		network, ok, err := app.findNetwork(id)
		if err != nil {
			app.redirectWithError(w, r, "/networks", err.Error())
			return
		}
		if !ok {
			app.redirectWithError(w, r, "/networks", "network not found: "+id)
			return
		}
		if network.Protected {
			app.redirectWithError(w, r, "/networks", "Docklight cannot delete its own or protected network: "+network.Name)
			return
		}
		if !network.Unused {
			app.redirectWithError(w, r, "/networks", "network is in use or protected: "+network.Name)
			return
		}
		ids = append(ids, network.ShortID)
		names = append(names, network.Name)
	}

	args := append([]string{"network", "rm"}, ids...)
	output, err := app.runDocker(args...)
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity("docker", "network delete selected", status, strings.Join(names, "\n")+"\n"+output)
	if err != nil {
		app.redirectWithError(w, r, "/networks", output)
		return
	}
	app.redirectWithMessage(w, r, "/networks", fmt.Sprintf("deleted %d selected networks", len(ids)))
}
