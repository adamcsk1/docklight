package main

import (
	"database/sql"
	"errors"
	"io"
	"log/slog"
	_ "modernc.org/sqlite"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var errTestBackend = errors.New("backend failed")

func newTestApp(t *testing.T, cfg Config) (*App, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActionTimeout == 0 {
		cfg.ActionTimeout = time.Minute
	}
	app := &App{cfg: cfg, db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := app.initDB(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return app, db
}

func installInventoryRunner(app *App) {
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -q", "docker container ls -aq":
			return "webid\n", nil
		case "docker container inspect webid":
			return `[{"ID":"webid123456789","Name":"/web","Image":"sha256:imgused","Config":{"Image":"nginx","Labels":{"com.docker.compose.project":"web","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{"usednet":{}}},"Mounts":[{"Type":"volume","Name":"usedvol"}]}]`, nil
		case "docker image ls -q":
			return "imgused\nunusedimg\n", nil
		case "docker image inspect imgused unusedimg":
			return `[{"ID":"sha256:imgused","RepoTags":["nginx:latest"],"Size":1},{"ID":"sha256:unusedimg","RepoTags":["unused:latest"],"Size":1}]`, nil
		case "docker volume ls -q":
			return "usedvol\nunusedvol\n", nil
		case "docker volume inspect usedvol unusedvol":
			return `[{"Name":"usedvol","Driver":"local"},{"Name":"unusedvol","Driver":"local"}]`, nil
		case "docker network ls -q":
			return "usedid\nunusedid\n", nil
		case "docker network inspect usedid unusedid":
			return `[{"ID":"usedid123456","Name":"usednet","Driver":"bridge","Scope":"local"},{"ID":"unusedid123456","Name":"unusednet","Driver":"bridge","Scope":"local"}]`, nil
		case "docker image rm unusedimg", "docker volume rm unusedvol", "docker network rm unusedid1234":
			return "deleted", nil
		}
		if strings.Contains(call, "compose") && strings.Contains(call, " ps") {
			return "[]", nil
		}
		if strings.Contains(call, "compose") && strings.Contains(call, "config --images") {
			return "nginx\n", nil
		}
		return "ok", nil
	}
}

func formRequest(rawURL string, values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, rawURL, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func requestWithPath(method, rawURL, key, value string, body *strings.Reader) *http.Request {
	var reader *strings.Reader
	if body != nil {
		reader = body
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, rawURL, reader)
	req.SetPathValue(key, value)
	return req
}

func requestWithCookie(base *http.Request, cookies []*http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodGet, base.URL.String(), nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	return req
}

func mapValues(kv ...string) url.Values {
	values := make(url.Values)
	for i := 0; i+1 < len(kv); i += 2 {
		values[kv[i]] = []string{kv[i+1]}
	}
	return values
}

type chownCall struct {
	Path string
	UID  int
	GID  int
}

func recordChown(t *testing.T) *[]chownCall {
	t.Helper()
	var calls []chownCall
	original := chownPath
	chownPath = func(path string, uid int, gid int) error {
		cleaned, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		calls = append(calls, chownCall{Path: cleaned, UID: uid, GID: gid})
		return nil
	}
	t.Cleanup(func() { chownPath = original })
	return &calls
}

func assertChownCall(t *testing.T, calls []chownCall, targetPath string, uid int, gid int) {
	t.Helper()
	targetPath, err := filepath.Abs(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Path == targetPath && call.UID == uid && call.GID == gid {
			return
		}
	}
	t.Fatalf("expected chown %s to %d:%d, got %#v", targetPath, uid, gid, calls)
}

func assertNoChownCall(t *testing.T, calls []chownCall, targetPath string, uid int, gid int) {
	t.Helper()
	targetPath, err := filepath.Abs(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Path == targetPath && call.UID == uid && call.GID == gid {
			t.Fatalf("did not expect chown %s to %d:%d, got %#v", targetPath, uid, gid, calls)
		}
	}
}

func installOnlyUsedInventoryRunner(app *App) {
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -q", "docker container ls -aq":
			return "webid\n", nil
		case "docker container inspect webid":
			return `[{"ID":"webid123456789","Name":"/web","Image":"sha256:imgused","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{"usednet":{}}},"Mounts":[{"Type":"volume","Name":"usedvol"}]}]`, nil
		case "docker image ls -q":
			return "imgused\n", nil
		case "docker image inspect imgused":
			return `[{"ID":"sha256:imgused","RepoTags":["nginx:latest"],"Size":1}]`, nil
		case "docker volume ls -q":
			return "usedvol\n", nil
		case "docker volume inspect usedvol":
			return `[{"Name":"usedvol","Driver":"local"}]`, nil
		case "docker network ls -q":
			return "usedid\n", nil
		case "docker network inspect usedid":
			return `[{"ID":"usedid123456","Name":"usednet","Driver":"bridge","Scope":"local"}]`, nil
		}
		return "", nil
	}
}
