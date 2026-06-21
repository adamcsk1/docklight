package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Addr           string
	BaseURL        string
	AdminUser      string
	AdminPassword  string
	SessionSecret  string
	StacksRoot     string
	StacksHostRoot string
	DBPath         string
	ActionTimeout  time.Duration
	CookieSecure   bool
}

func loadConfig() (Config, error) {
	stacksRoot := env("STACKS_ROOT", "/stacks")
	cfg := Config{
		Addr:           env("ADDR", ":8080"),
		BaseURL:        normalizeBaseURL(env("BASE_URL", "/")),
		AdminUser:      os.Getenv("ADMIN_USER"),
		AdminPassword:  os.Getenv("ADMIN_PASSWORD"),
		SessionSecret:  os.Getenv("SESSION_SECRET"),
		StacksRoot:     stacksRoot,
		StacksHostRoot: env("STACKS_HOST_ROOT", stacksRoot),
		DBPath:         env("DB_PATH", "/data/docklight.db"),
		ActionTimeout:  10 * time.Minute,
		CookieSecure:   parseBoolEnv("COOKIE_SECURE", false),
	}
	if cfg.AdminUser == "" || cfg.AdminPassword == "" || cfg.SessionSecret == "" {
		return cfg, errors.New("ADMIN_USER, ADMIN_PASSWORD, and SESSION_SECRET are required")
	}
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func parseBoolEnv(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func normalizeBaseURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return "/"
	}
	value = "/" + strings.Trim(value, "/")
	return value
}

func validateBaseURL(value string) error {
	if value == "/" {
		return nil
	}
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#%\\") || strings.Contains(value, "//") {
		return fmt.Errorf("BASE_URL must be a clean absolute path prefix: %q", value)
	}
	for _, segment := range strings.Split(strings.Trim(value, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("BASE_URL must not contain empty or dot path segments: %q", value)
		}
		for _, r := range segment {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				continue
			}
			return fmt.Errorf("BASE_URL contains unsupported character %q in %q", r, value)
		}
	}
	return nil
}
