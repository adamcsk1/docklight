package main

import (
	"os"
	"testing"
)

func TestConfigParsesProductionEnvironment(t *testing.T) {
	t.Setenv("ADDR", ":9090")
	t.Setenv("BASE_URL", "docklight")
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "pw")
	t.Setenv("SESSION_SECRET", "secret")
	t.Setenv("STACKS_ROOT", "/srv/stacks")
	t.Setenv("STACKS_HOST_ROOT", "/host/stacks")
	t.Setenv("DB_PATH", "/srv/data/app.db")
	t.Setenv("COOKIE_SECURE", "yes")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9090" || cfg.BaseURL != "/docklight" || cfg.StacksRoot != "/srv/stacks" || cfg.StacksHostRoot != "/host/stacks" || cfg.DBPath != "/srv/data/app.db" || !cfg.CookieSecure {
		t.Fatalf("unexpected config: %#v", cfg)
	}

	t.Setenv("BASE_URL", "/bad%2fpath")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid base URL to fail")
	}
}

func TestLoadConfigRequiresSecrets(t *testing.T) {
	for _, key := range []string{"ADMIN_USER", "ADMIN_PASSWORD", "SESSION_SECRET"} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected missing required auth config to fail")
	}
}
