package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"docklight/internal/management"
)

func TestMaintenanceExcludesOrdinaryOperationOutput(t *testing.T) {
	a := newTestApp(t)
	var jobRequests atomic.Int32
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/jobs/") {
			jobRequests.Add(1)
			id := "ordinary-job"
			if r.URL.Path != "/v1/jobs/current" {
				id = strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
			}
			_ = json.NewEncoder(w).Encode(management.Job{ID: id, Status: "running", Output: "ordinary output marker"})
			return
		}
		_ = json.NewEncoder(w).Encode(management.Inventory{})
	}))
	defer agent.Close()
	a.cfg.Hosts = []Host{{ID: "main", Name: "Main", URL: agent.URL, Token: strings.Repeat("t", 32), AllowHTTP: true, Master: true}}
	if err := configureHosts(a.cfg.Hosts); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO output_acknowledgements VALUES(?,?,?)`, "main", agent.URL, "acknowledged-job"); err != nil {
		t.Fatal(err)
	}
	cookie := session(t, a)
	get := func(path string) string {
		t.Helper()
		r := httptest.NewRequest("GET", "https://docklight.local/docklight"+path, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	for _, status := range []string{"running", "attention", "success"} {
		if !a.saveRun(&maintenanceRun{Status: status, Message: "fleet progress marker", Steps: []maintenanceStep{{Detail: "fleet output marker"}}}) {
			t.Fatal("cannot save fleet progress")
		}
		for _, query := range []string{"", "&job=ordinary-job", "&job=acknowledged-job&output=shown", "&job=../invalid"} {
			body := get("/?host=main&view=maintenance" + query)
			for _, absent := range []string{"data-operation-url", "data-operation-reopen", "Acknowledge output", "Show output", "ordinary output marker"} {
				if strings.Contains(body, absent) {
					t.Fatalf("maintenance rendered ordinary operation content %q", absent)
				}
			}
			if !strings.Contains(body, `data-maintenance-url="/docklight/maintenance?host=main"`) || !strings.Contains(body, "fleet progress marker") || !strings.Contains(body, "fleet output marker") {
				t.Fatal("maintenance lost its own progress, output or polling")
			}
		}
	}
	get("/maintenance?host=main")
	if jobRequests.Load() != 0 {
		t.Fatal("maintenance requested ordinary job state")
	}
	var acknowledgements int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM output_acknowledgements WHERE host=? AND endpoint=? AND job=?`, "main", agent.URL, "acknowledged-job").Scan(&acknowledgements); err != nil || acknowledgements != 1 {
		t.Fatalf("maintenance changed existing acknowledgement: %d %v", acknowledgements, err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM output_acknowledgements`).Scan(&acknowledgements); err != nil || acknowledgements != 1 {
		t.Fatalf("maintenance acknowledged another operation: %d %v", acknowledgements, err)
	}
	for _, view := range []string{"", "cleanup"} {
		body := get("/?host=main&view=" + view)
		if !strings.Contains(body, "ordinary output marker") || !strings.Contains(body, `data-operation-reopen hidden`) {
			t.Fatal("ordinary output lost or acknowledged on return from maintenance")
		}
		body = get("/?host=main&view=" + view + "&job=acknowledged-job")
		if !strings.Contains(body, "Show output") || strings.Contains(body, `data-operation-reopen hidden`) {
			t.Fatal("previously acknowledged output reopened on return from maintenance")
		}
	}
}
