package main

import (
	"io/fs"
	"net/http"
	"strings"
)

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(webFS, "static")

	app.handle(mux, "GET /static/", http.StripPrefix(app.withBase("/static/"), http.FileServer(http.FS(staticFS))), false)
	app.handle(mux, "GET /login", http.HandlerFunc(app.loginPage), false)
	app.handle(mux, "POST /login", http.HandlerFunc(app.login), false)
	app.handle(mux, "POST /logout", app.requireAuth(http.HandlerFunc(app.logout)), true)
	app.handle(mux, "GET /", app.requireAuth(http.HandlerFunc(app.dashboard)), true)
	app.handle(mux, "GET /usage/containers", app.requireAuth(http.HandlerFunc(app.containerUsage)), true)
	app.handle(mux, "GET /stacks/new", app.requireAuth(http.HandlerFunc(app.newStackPage)), true)
	app.handle(mux, "POST /stacks/new", app.requireAuth(http.HandlerFunc(app.createStack)), true)
	app.handle(mux, "GET /stacks/{name}/edit", app.requireAuth(http.HandlerFunc(app.editStackPage)), true)
	app.handle(mux, "POST /stacks/{name}/edit", app.requireAuth(http.HandlerFunc(app.updateStack)), true)
	app.handle(mux, "GET /stacks/{name}", app.requireAuth(http.HandlerFunc(app.stackDetail)), true)
	app.handle(mux, "POST /stacks/{name}/delete", app.requireAuth(http.HandlerFunc(app.deleteStack)), true)
	app.handle(mux, "POST /stacks/{name}/{action}", app.requireAuth(http.HandlerFunc(app.stackAction)), true)
	app.handle(mux, "GET /containers", app.requireAuth(http.HandlerFunc(app.containers)), true)
	app.handle(mux, "GET /containers/{id}/logs", app.requireAuth(http.HandlerFunc(app.containerLogsPage)), true)
	app.handle(mux, "GET /containers/{id}/logs/raw", app.requireAuth(http.HandlerFunc(app.containerLogsRaw)), true)
	app.handle(mux, "GET /containers/{id}/exec", app.requireAuth(http.HandlerFunc(app.containerExecPage)), true)
	app.handle(mux, "POST /containers/{id}/exec", app.requireAuth(http.HandlerFunc(app.containerExec)), true)
	app.handle(mux, "POST /containers/delete", app.requireAuth(http.HandlerFunc(app.deleteSelectedContainers)), true)
	app.handle(mux, "GET /external-containers", app.requireAuth(http.HandlerFunc(app.standaloneExternalContainersPage)), true)
	app.handle(mux, "GET /images", app.requireAuth(http.HandlerFunc(app.images)), true)
	app.handle(mux, "POST /images/delete", app.requireAuth(http.HandlerFunc(app.deleteSelectedImages)), true)
	app.handle(mux, "GET /volumes", app.requireAuth(http.HandlerFunc(app.volumes)), true)
	app.handle(mux, "POST /volumes/delete", app.requireAuth(http.HandlerFunc(app.deleteSelectedVolumes)), true)
	app.handle(mux, "GET /networks", app.requireAuth(http.HandlerFunc(app.networks)), true)
	app.handle(mux, "POST /networks/delete", app.requireAuth(http.HandlerFunc(app.deleteSelectedNetworks)), true)
	app.handle(mux, "GET /activity", app.requireAuth(http.HandlerFunc(app.activity)), true)
	app.handle(mux, "GET /activity/download", app.requireAuth(http.HandlerFunc(app.downloadActivity)), true)
	app.handle(mux, "GET /settings", app.requireAuth(http.HandlerFunc(app.settings)), true)
	app.handle(mux, "POST /settings", app.requireAuth(http.HandlerFunc(app.updateSettings)), true)
	app.handle(mux, "POST /settings/docklight/update", app.requireAuth(http.HandlerFunc(app.updateDocklight)), true)
	app.handle(mux, "GET /settings/verbose-log/download", app.requireAuth(http.HandlerFunc(app.downloadVerboseLog)), true)
	app.handle(mux, "POST /rescan", app.requireAuth(http.HandlerFunc(app.rescan)), true)
	app.handle(mux, "POST /database/rebuild", app.requireAuth(http.HandlerFunc(app.rebuildDatabase)), true)

	if app.cfg.BaseURL == "/" {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == app.cfg.BaseURL {
			http.Redirect(w, r, app.cfg.BaseURL+"/", http.StatusFound)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (app *App) handle(mux *http.ServeMux, pattern string, handler http.Handler, verbose bool) {
	if verbose {
		handler = app.withVerboseRequestLog(handler)
	}
	method, route, found := strings.Cut(pattern, " ")
	if !found {
		mux.Handle(app.withBase(pattern), handler)
		return
	}
	mux.Handle(method+" "+app.withBase(route), handler)
}

func (app *App) withBase(route string) string {
	if app.cfg.BaseURL == "/" {
		return route
	}
	if route == "/" {
		return app.cfg.BaseURL + "/"
	}
	return app.cfg.BaseURL + route
}
