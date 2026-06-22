package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

const maxFlashMessage = 3000

type PageData struct {
	Title               string
	Path                string
	Stacks              []Stack
	Stack               Stack
	StackForm           StackForm
	Activities          []Activity
	Containers          []DockerContainer
	Container           DockerContainer
	ContainerLog        string
	Images              []DockerImage
	Volumes             []DockerVolume
	Networks            []DockerNetwork
	Config              Config
	Settings            AppSettings
	RetentionDayOptions []int
	VerboseLogFiles     []string
	BuildDate           string
	Error               string
	Message             string
	CSRFToken           string
	UsageScript         bool
	ContainerLogScript  bool
}

func (app *App) loadTemplates() error {
	funcs := template.FuncMap{
		"base":                app.withBase,
		"eqInt":               func(a, b int) bool { return a == b },
		"active":              activePath,
		"metric":              metricValue,
		"usageContainerNames": usageContainerNames,
	}
	t, err := template.New("layout.html").Funcs(funcs).ParseFS(webFS, "templates/*.html")
	if err != nil {
		return err
	}
	app.templates = t
	return nil
}

func (app *App) render(w http.ResponseWriter, r *http.Request, name string, data PageData) {
	data.Path = r.URL.Path
	if app.cfg.BaseURL != "/" {
		data.Path = strings.TrimPrefix(r.URL.Path, app.cfg.BaseURL)
	}
	if data.Path == "" || data.Path[0] != '/' {
		data.Path = "/" + data.Path
	}
	if data.Path == "//" {
		data.Path = "/"
	}
	if key, message, ok := app.readFlashCookie(r); ok {
		switch key {
		case "error":
			if data.Error == "" {
				data.Error = message
			}
		case "message":
			if data.Message == "" {
				data.Message = message
			}
		}
		http.SetCookie(w, &http.Cookie{Name: "docklight_flash", Value: "", Path: app.cookiePath(), MaxAge: -1, HttpOnly: true, Secure: app.cookieSecure(r), SameSite: http.SameSiteLaxMode})
	}
	if cookie, err := r.Cookie("docklight_session"); err == nil && app.validSession(cookie.Value) {
		data.CSRFToken = app.csrfToken(cookie.Value)
	}
	if data.Error == "" {
		data.Error = r.URL.Query().Get("error")
	}
	if data.Message == "" {
		data.Message = r.URL.Query().Get("message")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.templates.ExecuteTemplate(w, name, data); err != nil {
		app.logger.Error("template render failed", "error", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func activePath(path, target string) bool {
	if target == "/" {
		return path == "/"
	}
	return path == target || strings.HasPrefix(path, target+"/")
}

func (app *App) redirectWithError(w http.ResponseWriter, r *http.Request, fallbackPath, message string) {
	app.redirectWithFlash(w, r, fallbackPath, "error", message)
}

func (app *App) redirectWithMessage(w http.ResponseWriter, r *http.Request, fallbackPath, message string) {
	app.redirectWithFlash(w, r, fallbackPath, "message", message)
}

func (app *App) redirectWithFlash(w http.ResponseWriter, r *http.Request, fallbackPath, key, message string) {
	target := r.Referer()
	if target == "" {
		target = app.withBase(fallbackPath)
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Path == "" || (parsed.IsAbs() && parsed.Host != r.Host) {
		parsed, _ = url.Parse(app.withBase(fallbackPath))
	}
	query := parsed.Query()
	query.Del("error")
	query.Del("message")
	parsed.RawQuery = query.Encode()
	app.setFlashCookie(w, r, key, message)
	http.Redirect(w, r, parsed.String(), http.StatusFound)
}

func (app *App) setFlashCookie(w http.ResponseWriter, r *http.Request, key, message string) {
	key = strings.TrimSpace(key)
	message = strings.TrimSpace(message)
	if len(message) > maxFlashMessage {
		message = message[:maxFlashMessage] + "\n... output truncated"
	}
	payload := key + "\n" + message
	http.SetCookie(w, &http.Cookie{Name: "docklight_flash", Value: app.signFlash(payload), Path: app.cookiePath(), MaxAge: 60, HttpOnly: true, Secure: app.cookieSecure(r), SameSite: http.SameSiteLaxMode})
}

func (app *App) readFlashCookie(r *http.Request) (string, string, bool) {
	cookie, err := r.Cookie("docklight_flash")
	if err != nil {
		return "", "", false
	}
	payload, ok := app.verifyFlash(cookie.Value)
	if !ok {
		return "", "", false
	}
	key, message, ok := strings.Cut(payload, "\n")
	return key, message, ok && (key == "error" || key == "message")
}

func (app *App) signFlash(payload string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, []byte(app.cfg.SessionSecret))
	mac.Write([]byte("flash:"))
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encoded + "." + sig
}

func (app *App) verifyFlash(value string) (string, bool) {
	encoded, sig, ok := strings.Cut(value, ".")
	if !ok || encoded == "" || sig == "" {
		return "", false
	}
	expected := app.signFlashBytes(encoded)
	if hmac.Equal([]byte(sig), []byte(expected)) {
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err == nil {
			return string(decoded), true
		}
	}
	return "", false
}

func (app *App) signFlashBytes(encoded string) string {
	mac := hmac.New(sha256.New, []byte(app.cfg.SessionSecret))
	mac.Write([]byte("flash:"))
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
