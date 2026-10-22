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

	"docklight/internal/management"
)

type hostView struct{ ID, Name string }
type containerGroup struct {
	Project    string
	Containers []management.Container
	Running    int
}
type resourceSection struct {
	Kind, Title string
	Items       []management.Resource
}
type PageData struct {
	Title, Error, CSRF, Base, HostID string
	Hosts                            []hostView
	Inventory                        management.Inventory
	ContainerGroups                  []containerGroup
	ResourceSections                 []resourceSection
	Jobs                             []management.Job
	Job                              *management.Job
	Refresh                          bool
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
	app.templates, err = template.New("").ParseFS(webFS, "templates/*.html")
	return err
}
func (app *App) render(w http.ResponseWriter, r *http.Request, name string, data PageData) {
	data.Base = app.withBase("")
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
	for _, asset := range []string{"app.css", "logo.ico"} {
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
	mux.Handle("POST "+app.withBase("/action"), app.requireAuth(http.HandlerFunc(app.action)))
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
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
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
	res, err := h.client.Do(req)
	if err != nil {
		return errors.New("agent unreachable or TLS verification failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if res.StatusCode == 409 {
			return errors.New("another operation or update is running on this host")
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
	d := PageData{Title: "Containers", HostID: h.ID}
	for _, host := range app.cfg.Hosts {
		d.Hosts = append(d.Hosts, hostView{host.ID, host.Name})
	}
	if err = h.request(r, "GET", "/v1/inventory", nil, &d.Inventory); err != nil {
		d.Error = err.Error()
	}
	d.ContainerGroups = groupContainers(d.Inventory.Containers)
	d.ResourceSections = []resourceSection{{Kind: "image", Title: "Images"}, {Kind: "volume", Title: "Volumes"}, {Kind: "network", Title: "Networks"}}
	for _, resource := range d.Inventory.Resources {
		for i := range d.ResourceSections {
			if d.ResourceSections[i].Kind == resource.Kind {
				d.ResourceSections[i].Items = append(d.ResourceSections[i].Items, resource)
			}
		}
	}
	if err = h.request(r, "GET", "/v1/jobs", nil, &d.Jobs); err != nil {
		d.Error = err.Error()
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
			d.Refresh = job.Status == "running" || job.Status == "updating"
		}
	}
	app.render(w, r, "manage.html", d)
}

func (app *App) action(w http.ResponseWriter, r *http.Request) {
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
	var j management.Job
	if err = h.request(r, "POST", "/v1/jobs", a, &j); err != nil {
		http.Error(w, err.Error()+". Submission outcome may be unknown; inspect operation history before retrying.", 502)
		return
	}
	log.Printf("submitted agent operation: host=%q kind=%q action=%q target=%q job=%q", h.ID, a.Kind, a.Operation, a.ID, j.ID)
	http.Redirect(w, r, app.withBase("/")+"?host="+url.QueryEscape(h.ID)+"&job="+url.QueryEscape(j.ID), http.StatusSeeOther)
}
