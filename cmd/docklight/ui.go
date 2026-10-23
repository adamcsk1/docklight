package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"time"

	"docklight/internal/buildinfo"
	"docklight/internal/management"
)

type hostView struct{ ID, Name string }

type agentRejection struct{ message string }

func (e *agentRejection) Error() string { return e.message }

type containerGroup struct {
	Project    string
	Containers []management.Container
	Running    int
}
type resourceSection struct {
	Kind, Title string
	Items       []management.Resource
	Unused      int
}
type PageData struct {
	Title, Error, CSRF, Base, HostID string
	UICommit, AgentCommit            string
	Hosts                            []hostView
	Inventory                        management.Inventory
	ContainerGroups                  []containerGroup
	ResourceSections                 []resourceSection
	Job                              *management.Job
	HideOutput                       bool
	Master                           bool
	Maintenance                      *maintenanceRun
	MaintenanceError                 string
	Blocked                          bool
}

func groupContainers(containers []management.Container) []containerGroup {
	var groups []containerGroup
	byProject := make(map[string]int)
	for _, container := range containers {
		i, ok := byProject[container.Project]
		if !ok {
			i = len(groups)
			byProject[container.Project] = i
			groups = append(groups, containerGroup{Project: container.Project})
		}
		groups[i].Containers = append(groups[i].Containers, container)
		if container.State == "running" {
			groups[i].Running++
		}
	}
	for i := range groups {
		sort.Slice(groups[i].Containers, func(a, b int) bool {
			x, y := groups[i].Containers[a], groups[i].Containers[b]
			if x.Name == y.Name {
				return x.ID < y.ID
			}
			return x.Name < y.Name
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Project == "" {
			return false
		}
		if groups[j].Project == "" {
			return true
		}
		return groups[i].Project < groups[j].Project
	})
	return groups
}

func (app *App) withBase(path string) string {
	if app.cfg.BaseURL == "/" {
		return path
	}
	return app.cfg.BaseURL + path
}
func (app *App) loadTemplates() error {
	var err error
	app.templates, err = template.New("").Funcs(template.FuncMap{"shortCommit": shortCommit}).ParseFS(webFS, "templates/*.html")
	return err
}

func shortCommit(commit string) string {
	if commit == "unavailable" {
		return commit
	}
	if commit == "" {
		return "unknown"
	}
	return commit[:min(7, len(commit))]
}

func (app *App) render(w http.ResponseWriter, r *http.Request, name string, data PageData) {
	data.Base = app.withBase("")
	data.UICommit = buildinfo.Commit
	if data.AgentCommit == "" {
		data.AgentCommit = "unavailable"
	}
	if cookie, err := r.Cookie("docklight_session"); err == nil {
		data.CSRF = app.csrfToken(cookie.Value)
	}
	var b bytes.Buffer
	if err := app.templates.ExecuteTemplate(&b, name, data); err != nil {
		http.Error(w, "cannot render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()
	for _, asset := range []string{"app.css", "app.js", "logo.ico"} {
		mux.HandleFunc("GET "+app.withBase("/static/"+asset), func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, webFS, "static/"+asset)
		})
	}
	mux.HandleFunc("GET "+app.withBase("/login"), app.loginPage)
	mux.HandleFunc("POST "+app.withBase("/login"), func(w http.ResponseWriter, r *http.Request) {
		if !validRequestOrigin(r) {
			http.Error(w, "bad request origin", 403)
			return
		}
		app.login(w, r)
	})
	mux.Handle("POST "+app.withBase("/logout"), app.requireAuth(http.HandlerFunc(app.logout)))
	mux.Handle("GET "+app.withBase("/{$}"), app.requireAuth(http.HandlerFunc(app.dashboard)))
	mux.Handle("GET "+app.withBase("/operation"), app.requireAuth(http.HandlerFunc(app.operation)))
	mux.Handle("GET "+app.withBase("/inventory"), app.requireAuth(http.HandlerFunc(app.inventory)))
	mux.Handle("POST "+app.withBase("/action"), app.requireAuth(http.HandlerFunc(app.action)))
	mux.Handle("POST "+app.withBase("/maintenance"), app.requireAuth(http.HandlerFunc(app.maintenanceStart)))
	mux.Handle("GET "+app.withBase("/maintenance"), app.requireAuth(http.HandlerFunc(app.maintenanceStatus)))
	if app.cfg.BaseURL != "/" {
		mux.HandleFunc("GET "+app.cfg.BaseURL, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != app.cfg.BaseURL {
				http.NotFound(w, r)
				return
			}
			http.Redirect(w, r, app.withBase("/"), 302)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		mux.ServeHTTP(w, r)
	})
}

func (app *App) host(id string) (*Host, error) {
	if id == "" && len(app.cfg.Hosts) > 0 {
		return &app.cfg.Hosts[0], nil
	}
	for i := range app.cfg.Hosts {
		if app.cfg.Hosts[i].ID == id {
			return &app.cfg.Hosts[i], nil
		}
	}
	return nil, errors.New("unknown host")
}

func (h *Host) request(r *http.Request, method, path string, body any, result any) error {
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), method, h.URL+path, &b)
	if err != nil {
		return errors.New("invalid agent request")
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Content-Type", "application/json")
	if owner, ok := r.Context().Value(coordinatorContextKey{}).(string); ok {
		req.Header.Set("X-Docklight-Coordinator", owner)
	}
	res, err := h.client.Do(req)
	if err != nil {
		return errors.New("agent unreachable or TLS verification failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Only fixed API rejections prove that submission did not start a job.
		switch res.StatusCode {
		case 400, 404, 405, 413:
			return &agentRejection{"agent rejected the action; no operation was submitted"}
		case 401, 403:
			return &agentRejection{"agent authentication rejected; check the server-side host token"}
		case 409:
			return &agentRejection{"another operation or update is running on this host"}
		}
		return fmt.Errorf("agent request failed (HTTP %d)", res.StatusCode)
	}
	if result == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(result)
}

func (app *App) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	h, err := app.host(r.URL.Query().Get("host"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	d := PageData{Title: "Containers", HostID: h.ID, HideOutput: r.URL.Query().Get("output") == "hidden"}
	for _, host := range app.cfg.Hosts {
		d.Hosts = append(d.Hosts, hostView{host.ID, host.Name})
	}
	app.populateMaintenance(r, &d)
	if d.Blocked {
		app.render(w, r, "manage.html", d)
		return
	}
	if id := r.URL.Query().Get("job"); id != "" {
		if !validID(id) {
			http.Error(w, "invalid job ID", 400)
			return
		}
		var job management.Job
		if err = h.request(r, "GET", "/v1/jobs/"+url.PathEscape(id), nil, &job); err != nil {
			d.Error = err.Error()
		} else {
			d.Job = &job
		}
	}
	// Read inventory after job status so a completed job cannot describe an earlier snapshot.
	if err = app.loadInventory(r, h, &d); err != nil {
		d.Error = err.Error()
	}
	app.render(w, r, "manage.html", d)
}

func (app *App) loadInventory(r *http.Request, h *Host, d *PageData) error {
	d.AgentCommit = "unavailable"
	if err := h.request(r, "GET", "/v1/inventory", nil, &d.Inventory); err != nil {
		return err
	}
	d.AgentCommit = d.Inventory.Commit
	if d.AgentCommit == "" {
		d.AgentCommit = "unknown"
	}
	d.ContainerGroups = groupContainers(d.Inventory.Containers)
	d.ResourceSections = []resourceSection{{Kind: "image", Title: "Images"}, {Kind: "volume", Title: "Volumes"}, {Kind: "network", Title: "Networks"}}
	for _, resource := range d.Inventory.Resources {
		for i := range d.ResourceSections {
			if d.ResourceSections[i].Kind == resource.Kind {
				d.ResourceSections[i].Items = append(d.ResourceSections[i].Items, resource)
				if !resource.Protected && len(resource.UsedBy) == 0 {
					d.ResourceSections[i].Unused++
				}
			}
		}
	}
	return nil
}

func (app *App) inventory(w http.ResponseWriter, r *http.Request) {
	if !app.allowManagement(w, r) {
		return
	}
	hostID := r.URL.Query().Get("host")
	if hostID == "" {
		http.Error(w, "explicit host required", http.StatusBadRequest)
		return
	}
	h, err := app.host(hostID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	d := PageData{HostID: h.ID}
	if err = app.loadInventory(r, h, &d); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	app.render(w, r, "inventory", d)
}

func (app *App) operation(w http.ResponseWriter, r *http.Request) {
	id, hostID := r.URL.Query().Get("job"), r.URL.Query().Get("host")
	if !validID(id) || hostID == "" {
		http.Error(w, "explicit host and valid job ID required", http.StatusBadRequest)
		return
	}
	h, err := app.host(hostID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var job management.Job
	if err = h.request(r.WithContext(ctx), "GET", "/v1/jobs/"+url.PathEscape(id), nil, &job); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(job)
}

func (app *App) action(w http.ResponseWriter, r *http.Request) {
	if !app.allowManagement(w, r) {
		return
	}
	if app.masterHost() != nil {
		r = r.WithContext(context.WithValue(r.Context(), coordinatorContextKey{}, app.coordinator))
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	// No default host for mutations: never silently fall back to the local machine.
	if r.PostForm.Get("host") == "" {
		http.Error(w, "explicit host required", 400)
		return
	}
	h, err := app.host(r.PostForm.Get("host"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a := management.Action{Kind: r.PostForm.Get("kind"), ID: r.PostForm.Get("id"), Operation: r.PostForm.Get("operation"), Scope: r.PostForm.Get("scope"), Confirmed: r.PostForm.Get("confirmed") == "yes"}
	if err := management.ValidateAction(a); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var j management.Job
	if err = h.request(r, "POST", "/v1/jobs", a, &j); err != nil {
		var rejection *agentRejection
		if errors.As(err, &rejection) {
			http.Error(w, rejection.Error(), http.StatusBadGateway)
			return
		}
		http.Error(w, err.Error()+". Submission outcome may be unknown; inspect the host and agent logs before retrying.", 502)
		return
	}
	log.Printf("submitted agent operation: host=%q kind=%q action=%q target=%q job=%q", h.ID, a.Kind, a.Operation, a.ID, j.ID)
	http.Redirect(w, r, app.withBase("/")+"?host="+url.QueryEscape(h.ID)+"&job="+url.QueryEscape(j.ID), http.StatusSeeOther)
}
