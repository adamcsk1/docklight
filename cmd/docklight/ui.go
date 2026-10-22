package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"time"

	"docklight/internal/management"
)

type hostView struct{ ID, Name string }
type historyEntry struct{ Host, Action, Status, Output, Created string }
type PageData struct {
	Title, Error, CSRF, Base, HostID string
	Hosts                            []hostView
	Inventory                        management.Inventory
	Jobs                             []management.Job
	Job                              *management.Job
	History                          []historyEntry
	Refresh                          bool
}

func (app *App) withBase(path string) string {
	if app.cfg.BaseURL == "/" {
		return path
	}
	return app.cfg.BaseURL + path
}
func (app *App) loadTemplates() error {
	var err error
	app.templates, err = template.New("").ParseFS(webFS, "templates/login.html", "templates/manage.html")
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
	mux.HandleFunc("GET "+app.withBase("/static/manage.css"), func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("static/manage.css")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(b)
	})
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
		// Allow the reported inline style, including attributes, without permitting arbitrary inline CSS.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-hashes' 'sha256-vKC/3e+58wt3AEEnUImhhkZlNJYe2iGGM4zT9jQL6x0='; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
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
	d := PageData{Title: "Docklight", HostID: h.ID}
	for _, host := range app.cfg.Hosts {
		d.Hosts = append(d.Hosts, hostView{host.ID, host.Name})
	}
	if err = h.request(r, "GET", "/v1/inventory", nil, &d.Inventory); err != nil {
		d.Error = err.Error()
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
	rows, err := app.db.Query(`SELECT stack_name,action,status,output,created_at FROM activity_log ORDER BY id DESC LIMIT 100`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var entry historyEntry
			if rows.Scan(&entry.Host, &entry.Action, &entry.Status, &entry.Output, &entry.Created) == nil {
				d.History = append(d.History, entry)
			}
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
	app.logActivity(h.ID, a.Kind+" "+a.Operation, "submitted", a.ID+" job="+j.ID)
	http.Redirect(w, r, app.withBase("/")+"?host="+url.QueryEscape(h.ID)+"&job="+url.QueryEscape(j.ID), http.StatusSeeOther)
}
