package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Addr, BaseURL, AdminPassword, SessionSecret, DBPath string
	CookieSecure                                        bool
	Hosts                                               []Host
}
type Host struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`
	Token     string `yaml:"token"`
	CAFile    string `yaml:"ca_file"`
	AllowHTTP bool   `yaml:"allow_http"`
	client    *http.Client
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func loadConfig() (Config, error) {
	c := Config{Addr: env("ADDR", ":8080"), BaseURL: "/" + strings.Trim(env("BASE_URL", "/"), "/"), AdminPassword: os.Getenv("ADMIN_PASSWORD"), SessionSecret: os.Getenv("SESSION_SECRET"), DBPath: env("DB_PATH", "/data/sessions.db"), CookieSecure: env("COOKIE_SECURE", "false") == "true"}
	if c.AdminPassword == "" || len(c.SessionSecret) < 32 {
		return c, errors.New("ADMIN_PASSWORD and SESSION_SECRET (at least 32 characters) are required")
	}
	if c.BaseURL != "/" {
		for _, segment := range strings.Split(strings.TrimPrefix(c.BaseURL, "/"), "/") {
			if !validID(segment) {
				return c, errors.New("BASE_URL must contain only letters, digits, hyphens and underscores")
			}
		}
	}
	f, err := os.Open(env("HOSTS_FILE", "/data/hosts.yml"))
	if err != nil {
		return c, err
	}
	defer f.Close()
	var hosts struct {
		Hosts []Host `yaml:"hosts"`
	}
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if err = d.Decode(&hosts); err != nil {
		return c, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("unexpected hosts configuration data")
	}
	c.Hosts = hosts.Hosts
	return c, configureHosts(c.Hosts)
}
func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func configureHosts(hosts []Host) error {
	if len(hosts) == 0 {
		return errors.New("at least one agent host is required")
	}
	seen := map[string]bool{}
	for i := range hosts {
		h := &hosts[i]
		if !validID(h.ID) || seen[h.ID] || h.Name == "" || len(h.Token) < 32 || strings.ContainsAny(h.Token, "\r\n") {
			return errors.New("hosts require unique IDs, names and tokens of at least 32 characters")
		}
		seen[h.ID] = true
		u, err := url.Parse(h.URL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" || u.Scheme != "https" && !(u.Scheme == "http" && h.AllowHTTP) {
			return fmt.Errorf("host %s requires HTTPS (or explicit allow_http on isolated network), with no URL credentials/path/query", h.ID)
		}
		h.URL = strings.TrimRight(h.URL, "/")
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if h.CAFile != "" {
			pem, err := os.ReadFile(h.CAFile)
			if err != nil {
				return err
			}
			pool, err := x509.SystemCertPool()
			if err != nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return errors.New("invalid agent CA certificate")
			}
			tlsConfig.RootCAs = pool
		}
		h.client = &http.Client{Timeout: 45 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig, ResponseHeaderTimeout: 35 * time.Second, IdleConnTimeout: time.Minute, MaxIdleConnsPerHost: 2}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return nil
}
