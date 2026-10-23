package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMasterHostConfiguration(t *testing.T) {
	for _, flags := range [][]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		hosts := []Host{{ID: "main", Name: "Main", URL: "http://main", Token: strings.Repeat("s", 32), AllowHTTP: true, Master: flags[0]}, {ID: "remote", Name: "Remote", URL: "http://remote", Token: strings.Repeat("t", 32), AllowHTTP: true, Master: flags[1]}}
		err := configureHosts(hosts)
		if flags[0] && flags[1] {
			if err == nil || !strings.Contains(err.Error(), "main, remote") {
				t.Fatalf("multiple masters accepted: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	var h Host
	if err := yaml.Unmarshal([]byte("id: main\nmaster: nonsense\n"), &h); err == nil {
		t.Fatal("invalid master boolean accepted")
	}
}

func TestLoadConfigBlocksTwoMastersBeforeServing(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "password")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 32))
	file := filepath.Join(t.TempDir(), "hosts.yml")
	t.Setenv("HOSTS_FILE", file)
	content := "hosts:\n  - id: main\n    master: true\n  - id: second\n    master: true\n"
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "main, second") {
		t.Fatalf("bad startup config: %v", err)
	}
}
