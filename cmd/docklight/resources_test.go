package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSelectedContainerDeleteRequiresForceForRunningContainers(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/web","Image":"sha256:image","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}]`, nil
		case "docker container rm --force abc123456789":
			return "removed", nil
		default:
			return "", nil
		}
	}

	values := url.Values{"ids": {"abc123456789"}}
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/containers/delete", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.deleteSelectedContainers(rec, req)
	if !strings.Contains(rec.Result().Header.Get("Set-Cookie"), "docklight_flash") {
		t.Fatal("expected error flash cookie")
	}
	if strings.Contains(strings.Join(calls, "\n"), "container rm") {
		t.Fatalf("container rm should not run without force, calls: %v", calls)
	}

	values.Set("force", "on")
	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/containers/delete", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	app.deleteSelectedContainers(rec, req)
	if !strings.Contains(strings.Join(calls, "\n"), "docker container rm --force abc123456789") {
		t.Fatalf("expected forced container rm, calls: %v", calls)
	}
}

func TestDeleteNetworkRejectsProtectedNetwork(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker network ls -q":
			return "bridgeid\n", nil
		case "docker container ls -aq":
			return "", nil
		case "docker network inspect bridgeid":
			return `[{"ID":"bridgeid123456","Name":"bridge","Driver":"bridge","Scope":"local"}]`, nil
		case "docker network rm bridgeid":
			t.Fatal("protected bridge network should not be removed")
		}
		return "", nil
	}

	req := formRequest("http://docklight.local/networks/delete", url.Values{"ids": {"bridge"}})
	rec := httptest.NewRecorder()
	app.deleteSelectedNetworks(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
}

func TestProtectedDocklightContainerCannotBeStoppedOrDeleted(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/docklight","Image":"sha256:docklightimage","Config":{"Image":"docklight:local","Labels":{"com.docker.compose.project":"docklight","com.docker.compose.service":"docklight"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}]`, nil
		}
		return "", nil
	}

	values := url.Values{"force": {"on"}, "ids": {"abc123456789"}}
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/containers/delete", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.deleteSelectedContainers(rec, req)
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "container rm") || rec.Code != http.StatusFound {
		t.Fatalf("expected protected container delete to be blocked, code=%d calls=%s", rec.Code, joined)
	}

}

func TestProtectedDocklightImageCannotBeDeletedWithForce(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker image ls -q":
			return "docklightimage\n", nil
		case "docker container ls -aq":
			return "docklightcontainer\n", nil
		case "docker container inspect docklightcontainer":
			return `[{"ID":"docklightcontainer","Name":"/docklight","Image":"sha256:docklightimage","Config":{"Image":"docklight:local","Labels":{"com.docker.compose.project":"docklight","com.docker.compose.service":"docklight"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"}}]`, nil
		case "docker image inspect docklightimage":
			return `[{"ID":"sha256:docklightimage","RepoTags":["docklight:local"],"RepoDigests":["docklight@sha256:old"],"Size":123,"Created":"2026-01-01T00:00:00Z"}]`, nil
		case "docker image rm --force docklightimage":
			t.Fatal("protected image should not be removed")
		}
		return "", nil
	}

	req := formRequest("http://docklight.local/images/delete", url.Values{"force": {"on"}, "ids": {"docklightimage"}})
	rec := httptest.NewRecorder()
	app.deleteSelectedImages(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected protected image redirect, got %d", rec.Code)
	}

}

func TestResourceDeleteHandlersSuccessPaths(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	installInventoryRunner(app)

	for _, tc := range []struct {
		name    string
		method  func(http.ResponseWriter, *http.Request)
		request *http.Request
		want    string
	}{
		{name: "delete selected images", method: app.deleteSelectedImages, request: formRequest("http://docklight.local/images/delete", url.Values{"ids": {"unusedimg"}}), want: "deleted 1 selected images"},
		{name: "delete selected volumes", method: app.deleteSelectedVolumes, request: formRequest("http://docklight.local/volumes/delete", url.Values{"ids": {"unusedvol"}}), want: "deleted 1 selected volumes"},
		{name: "delete selected networks", method: app.deleteSelectedNetworks, request: formRequest("http://docklight.local/networks/delete", url.Values{"ids": {"unusednet"}}), want: "deleted 1 selected networks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.method(rec, tc.request)
			if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Set-Cookie"), "docklight_flash") {
				t.Fatalf("expected redirect with flash for %s, code=%d", tc.name, rec.Code)
			}
			if key, message, ok := app.readFlashCookie(requestWithCookie(tc.request, rec.Result().Cookies())); !ok || key != "message" || !strings.Contains(message, tc.want) {
				t.Fatalf("expected success flash containing %q, key=%q ok=%v message=%q", tc.want, key, ok, message)
			}
		})
	}
}

func TestMissingLookupsAndStackImageIDs(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	installInventoryRunner(app)
	if _, ok, err := app.findImage("missing"); err != nil || ok {
		t.Fatalf("expected missing image, ok=%v err=%v", ok, err)
	}
	if _, ok, err := app.findVolume("missing"); err != nil || ok {
		t.Fatalf("expected missing volume, ok=%v err=%v", ok, err)
	}
	if _, ok, err := app.findNetwork("missing"); err != nil || ok {
		t.Fatalf("expected missing network, ok=%v err=%v", ok, err)
	}
	containers, err := app.dockerContainerResources(true)
	ids := stackImageIDsFromContainers("web", containers)
	if err != nil || len(ids) != 1 || ids[0] != "sha256:imgused" {
		t.Fatalf("unexpected stack image ids %#v err=%v", ids, err)
	}
}

func TestResourceHandlersRejectInvalidRequestsBeforeDockerMutation(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	installInventoryRunner(app)

	for _, tc := range []struct {
		name    string
		method  func(http.ResponseWriter, *http.Request)
		request *http.Request
	}{
		{name: "container no selection", method: app.deleteSelectedContainers, request: formRequest("http://docklight.local/containers/delete", url.Values{})},
		{name: "container missing", method: app.deleteSelectedContainers, request: formRequest("http://docklight.local/containers/delete", url.Values{"ids": {"missing"}})},
		{name: "image no selection", method: app.deleteSelectedImages, request: formRequest("http://docklight.local/images/delete", url.Values{})},
		{name: "image missing", method: app.deleteSelectedImages, request: formRequest("http://docklight.local/images/delete", url.Values{"ids": {"missing"}})},
		{name: "volume no selection", method: app.deleteSelectedVolumes, request: formRequest("http://docklight.local/volumes/delete", url.Values{})},
		{name: "volume missing", method: app.deleteSelectedVolumes, request: formRequest("http://docklight.local/volumes/delete", url.Values{"ids": {"missing"}})},
		{name: "network no selection", method: app.deleteSelectedNetworks, request: formRequest("http://docklight.local/networks/delete", url.Values{})},
		{name: "network missing", method: app.deleteSelectedNetworks, request: formRequest("http://docklight.local/networks/delete", url.Values{"ids": {"missing"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.method(rec, tc.request)
			if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Set-Cookie"), "docklight_flash") {
				t.Fatalf("expected redirect with error flash, code=%d", rec.Code)
			}
		})
	}
}

func TestDockerInventoryPagesReturnBackendErrors(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if err := app.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) { return "boom", errTestBackend }
	for _, tc := range []struct {
		name   string
		method func(http.ResponseWriter, *http.Request)
		url    string
	}{
		{name: "containers", method: app.containers, url: "http://docklight.local/containers"},
		{name: "images", method: app.images, url: "http://docklight.local/images"},
		{name: "volumes", method: app.volumes, url: "http://docklight.local/volumes"},
		{name: "networks", method: app.networks, url: "http://docklight.local/networks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.method(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected backend error, got %d", rec.Code)
			}
		})
	}
}

func TestResourceMutationDockerFailuresAreReported(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq":
			return "", nil
		case "docker image ls -q":
			return "unusedimg\n", nil
		case "docker image inspect unusedimg":
			return `[{"ID":"sha256:unusedimg","RepoTags":["unused:latest"],"Size":1}]`, nil
		case "docker volume ls -q":
			return "unusedvol\n", nil
		case "docker volume inspect unusedvol":
			return `[{"Name":"unusedvol","Driver":"local"}]`, nil
		case "docker network ls -q":
			return "unusedid\n", nil
		case "docker network inspect unusedid":
			return `[{"ID":"unusedid123456","Name":"unusednet","Driver":"bridge","Scope":"local"}]`, nil
		}
		if strings.Contains(call, " rm ") {
			return "rm failed", errTestBackend
		}
		return "", nil
	}

	for _, tc := range []struct {
		name    string
		method  func(http.ResponseWriter, *http.Request)
		request *http.Request
	}{
		{name: "delete selected image", method: app.deleteSelectedImages, request: formRequest("http://docklight.local/images/delete", url.Values{"ids": {"unusedimg"}})},
		{name: "delete selected volume", method: app.deleteSelectedVolumes, request: formRequest("http://docklight.local/volumes/delete", url.Values{"ids": {"unusedvol"}})},
		{name: "delete selected network", method: app.deleteSelectedNetworks, request: formRequest("http://docklight.local/networks/delete", url.Values{"ids": {"unusednet"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.method(rec, tc.request)
			if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Set-Cookie"), "docklight_flash") {
				t.Fatalf("expected failure redirect with flash, code=%d", rec.Code)
			}
		})
	}
}

func TestResourceInventoryEmptyAndMalformedDockerOutput(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		return "\n", nil
	}
	if containers, err := app.dockerContainerResources(false); err != nil || len(containers) != 0 {
		t.Fatalf("expected empty container list without error, got %#v err=%v", containers, err)
	}
	if images, err := app.dockerImages(); err != nil || images != nil {
		t.Fatalf("expected empty image list without error, got %#v err=%v", images, err)
	}
	if volumes, err := app.dockerVolumes(); err != nil || volumes != nil {
		t.Fatalf("expected empty volume list without error, got %#v err=%v", volumes, err)
	}
	if networks, err := app.dockerNetworks(); err != nil || networks != nil {
		t.Fatalf("expected empty network list without error, got %#v err=%v", networks, err)
	}

	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -q", "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return "not-json", nil
		}
		return "", nil
	}
	if _, err := app.dockerContainerResources(false); err == nil {
		t.Fatal("expected malformed container inspect JSON to fail")
	}
}

func TestResourceMutationBadFormsAndNoUnusedPrune(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	installOnlyUsedInventoryRunner(app)

	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/containers/delete", nil)
	req.URL.RawQuery = "%zz"
	rec := httptest.NewRecorder()
	app.deleteSelectedContainers(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Set-Cookie"), "docklight_flash") {
		t.Fatalf("expected bad container form redirect with flash, code=%d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://docklight.local/networks/delete", nil)
	req.URL.RawQuery = "%zz"
	rec = httptest.NewRecorder()
	app.deleteSelectedNetworks(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Set-Cookie"), "docklight_flash") {
		t.Fatalf("expected bad network form redirect with flash, code=%d", rec.Code)
	}

}

func TestResourceRefSortingAndProtectedContainerMatching(t *testing.T) {
	refs := []ResourceRef{
		{Name: "z", Stack: "z"},
		{Name: "b", Stack: "a"},
		{Name: "a", Stack: "a", Protected: true},
	}
	sortResourceRefs(refs)
	if refs[0].Name != "b" || refs[1].Name != "z" || !refs[2].Protected {
		t.Fatalf("unexpected sorted refs: %#v", refs)
	}
	containers := []DockerContainer{{Stack: "external:web", Protected: true}, {Stack: "other", Protected: true}}
	if !stackHasProtectedContainer("web", containers) || stackHasProtectedContainer("missing", containers) {
		t.Fatalf("unexpected protected container match result")
	}
}

func TestResourceMutationContainerAndForceImageBranches(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	installOnlyUsedInventoryRunner(app)

	req := formRequest("http://docklight.local/containers/delete", url.Values{"ids": {"webid123456"}})
	rec := httptest.NewRecorder()
	app.deleteSelectedContainers(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected running container force redirect, got %d", rec.Code)
	}

	app, _ = newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	installOnlyUsedInventoryRunner(app)
	req = formRequest("http://docklight.local/images/delete", url.Values{"force": {"on"}, "ids": {"imgused"}})
	rec = httptest.NewRecorder()
	app.deleteSelectedImages(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected forced in-use image delete redirect, got %d", rec.Code)
	}
}

func TestResourceInspectJSONErrorsForImagesVolumesAndNetworks(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker image ls -q":
			return "img1\n", nil
		case "docker container ls -aq":
			return "", nil
		case "docker image inspect img1":
			return "not-json", nil
		}
		return "", nil
	}
	if _, err := app.dockerImages(); err == nil {
		t.Fatal("expected malformed image inspect JSON to fail")
	}

	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker volume ls -q":
			return "vol1\n", nil
		case "docker container ls -aq":
			return "", nil
		case "docker volume inspect vol1":
			return "not-json", nil
		}
		return "", nil
	}
	if _, err := app.dockerVolumes(); err == nil {
		t.Fatal("expected malformed volume inspect JSON to fail")
	}

	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker network ls -q":
			return "net1\n", nil
		case "docker container ls -aq":
			return "", nil
		case "docker network inspect net1":
			return "not-json", nil
		}
		return "", nil
	}
	if _, err := app.dockerNetworks(); err == nil {
		t.Fatal("expected malformed network inspect JSON to fail")
	}
}

func TestDockerResourceInventoryClassifiesUsageAndProtection(t *testing.T) {
	app, db := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	if _, err := db.Exec(`INSERT INTO stacks (name, relative_path, compose_file) VALUES ('web', 'web', 'compose.yml')`); err != nil {
		t.Fatal(err)
	}
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "docker container ls -aq", "docker container ls -q":
			return "webid\ndockid\n", nil
		case "docker container inspect webid dockid":
			return `[
{"ID":"webid123456789","Name":"/web","Image":"sha256:imgweb","Config":{"Image":"nginx","Labels":{"com.docker.compose.project":"web","com.docker.compose.service":"app"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z","Health":{"Status":"healthy"}},"NetworkSettings":{"Ports":{"80/tcp":[{"HostIP":"127.0.0.1","HostPort":"8080"}],"443/tcp":null},"Networks":{"frontend":{}}},"Mounts":[{"Type":"volume","Name":"webdata"}]},
{"ID":"dockid123456789","Name":"/docklight","Image":"sha256:imgdock","Config":{"Image":"docklight:local","Labels":{"com.docker.compose.project":"docklight","com.docker.compose.service":"docklight"}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{"docknet":{}}},"Mounts":[{"Type":"volume","Name":"dockdata"}]}
]`, nil
		case "docker image ls -q":
			return "imgweb\nimgdock\nunusedimg\n", nil
		case "docker image inspect imgweb imgdock unusedimg":
			return `[{"ID":"sha256:imgweb","RepoTags":["nginx:latest"],"Size":2048,"Created":"2026-01-01T00:00:00Z"},{"ID":"sha256:imgdock","RepoTags":["docklight:local"],"Size":4096,"Created":"2026-01-01T00:00:00Z"},{"ID":"sha256:unusedimg","RepoTags":[],"Size":512,"Created":"bad-time"}]`, nil
		case "docker volume ls -q":
			return "webdata\ndockdata\nunusedvol\n", nil
		case "docker volume inspect webdata dockdata unusedvol":
			return `[{"Name":"webdata","Driver":"local","Mountpoint":"/var/lib/docker/volumes/webdata","CreatedAt":"2026-01-01T00:00:00Z"},{"Name":"dockdata","Driver":"local","Mountpoint":"/var/lib/docker/volumes/dockdata","CreatedAt":"2026-01-01T00:00:00Z"},{"Name":"unusedvol","Driver":"local","Mountpoint":"/var/lib/docker/volumes/unusedvol","CreatedAt":"bad-time"}]`, nil
		case "docker network ls -q":
			return "frontid\nbridgeid\ndockid\n", nil
		case "docker network inspect frontid bridgeid dockid":
			return `[{"ID":"frontid123456","Name":"frontend","Driver":"bridge","Scope":"local","Attachable":true},{"ID":"bridgeid123456","Name":"bridge","Driver":"bridge","Scope":"local"},{"ID":"dockid123456","Name":"docknet","Driver":"bridge","Scope":"local"}]`, nil
		}
		return "", nil
	}

	containers, err := app.dockerContainerResources(false)
	if err != nil || len(containers) != 2 || containers[0].Ports != "127.0.0.1:8080->80/tcp, 443/tcp" || containers[1].Name != "docklight" || !containers[1].Protected {
		t.Fatalf("unexpected containers %#v err=%v", containers, err)
	}
	images, err := app.dockerImages()
	if err != nil || len(images) != 3 || !images[0].Unused || !images[2].Protected {
		t.Fatalf("unexpected images %#v err=%v", images, err)
	}
	volumes, err := app.dockerVolumes()
	if err != nil || len(volumes) != 3 || !volumes[0].Unused || !volumes[2].Protected {
		t.Fatalf("unexpected volumes %#v err=%v", volumes, err)
	}
	networks, err := app.dockerNetworks()
	if err != nil || len(networks) != 3 || !networks[2].Protected {
		t.Fatalf("unexpected networks %#v err=%v", networks, err)
	}
	if _, ok, err := app.findImage("unusedimg"); err != nil || !ok {
		t.Fatalf("expected image lookup to work, ok=%v err=%v", ok, err)
	}
	if _, ok, err := app.findVolume("unusedvol"); err != nil || !ok {
		t.Fatalf("expected volume lookup to work, ok=%v err=%v", ok, err)
	}
	if _, ok, err := app.findNetwork("frontend"); err != nil || !ok {
		t.Fatalf("expected network lookup to work, ok=%v err=%v", ok, err)
	}
}

func TestResourceDeleteHandlersProtectInUseAssets(t *testing.T) {
	app, _ := newTestApp(t, Config{BaseURL: "/", StacksRoot: t.TempDir(), SessionSecret: "secret", AdminUser: "admin", AdminPassword: "pw"})
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "docker container ls -aq":
			return "abc123\n", nil
		case "docker container inspect abc123":
			return `[{"ID":"abc123456789","Name":"/web","Image":"sha256:imgused","Config":{"Image":"nginx","Labels":{}},"State":{"Status":"running","StartedAt":"2026-01-01T00:00:00Z"},"NetworkSettings":{"Ports":{},"Networks":{"usednet":{}}},"Mounts":[{"Type":"volume","Name":"usedvol"}]}]`, nil
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
		return "", nil
	}

	values := url.Values{"ids": {"imgused"}}
	req := httptest.NewRequest(http.MethodPost, "http://docklight.local/images/delete", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.deleteSelectedImages(rec, req)
	if strings.Contains(strings.Join(calls, "\n"), "image rm imgused") {
		t.Fatal("in-use image should not be removed without force")
	}

	req = formRequest("http://docklight.local/volumes/delete", url.Values{"ids": {"usedvol"}})
	rec = httptest.NewRecorder()
	app.deleteSelectedVolumes(rec, req)
	if strings.Contains(strings.Join(calls, "\n"), "volume rm usedvol") {
		t.Fatal("in-use volume should not be removed")
	}

	req = formRequest("http://docklight.local/networks/delete", url.Values{"ids": {"usednet"}})
	rec = httptest.NewRecorder()
	app.deleteSelectedNetworks(rec, req)
	if strings.Contains(strings.Join(calls, "\n"), "network rm usedid") {
		t.Fatal("in-use network should not be removed")
	}
}
