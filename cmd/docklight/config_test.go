package main

import (
	"os"
	"runtime"
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
	if runtime.GOOS != "windows" {
		t.Setenv("STACKS_FILE_OWNER", "1000:1001")
		t.Setenv("DATA_FILE_OWNER", "2000:2001")
	}
	t.Setenv("DB_PATH", "/srv/data/app.db")
	t.Setenv("COOKIE_SECURE", "yes")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9090" || cfg.BaseURL != "/docklight" || cfg.StacksRoot != "/srv/stacks" || cfg.StacksHostRoot != "/host/stacks" || cfg.DBPath != "/srv/data/app.db" || !cfg.CookieSecure {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if runtime.GOOS != "windows" && (cfg.StackFileOwner == nil || cfg.StackFileOwner.UID != 1000 || cfg.StackFileOwner.GID != 1001) {
		t.Fatalf("unexpected stack file owner: %#v", cfg.StackFileOwner)
	}
	if runtime.GOOS != "windows" && (cfg.DataFileOwner == nil || cfg.DataFileOwner.UID != 2000 || cfg.DataFileOwner.GID != 2001) {
		t.Fatalf("unexpected data file owner: %#v", cfg.DataFileOwner)
	}

	t.Setenv("BASE_URL", "/bad%2fpath")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid base URL to fail")
	}
}

func TestLoadConfigRejectsInvalidDataFileOwner(t *testing.T) {
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "pw")
	t.Setenv("SESSION_SECRET", "secret")
	t.Setenv("DATA_FILE_OWNER", "user:group")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid DATA_FILE_OWNER to fail")
	}
}

func TestLoadConfigRejectsInvalidStackFileOwner(t *testing.T) {
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "pw")
	t.Setenv("SESSION_SECRET", "secret")
	t.Setenv("STACKS_FILE_OWNER", "user:group")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid STACKS_FILE_OWNER to fail")
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
