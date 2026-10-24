package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"docklight/internal/management"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func checkHealth(url string, secure bool) error {
	transport := &http.Transport{}
	if secure {
		// Deployment certificates need not include localhost for this credential-free local probe.
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("health status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 3))
	if err != nil || string(body) != "ok" {
		return errors.New("invalid health response")
	}
	return nil
}

func healthURL(scheme, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", errors.New("invalid health address")
	}
	if ip := net.ParseIP(host); host == "" || host == "*" || ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func healthProbe() error {
	scheme, secure := "http", false
	if os.Getenv("TLS_CERT") != "" {
		scheme, secure = "https", true
	}
	url, err := healthURL(scheme, env("ADDR", ":8081"))
	if err != nil {
		return err
	}
	return checkHealth(url, secure)
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		if err := healthProbe(); err != nil {
			os.Exit(1)
		}
		return
	}
	e := &management.Engine{Root: env("STACKS_ROOT", "/stacks"), DataDir: env("DATA_DIR", "/data"), Timeout: 10 * time.Minute}
	s, err := management.NewServer(e, os.Getenv("AGENT_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: env("ADDR", ":8081"), Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: time.Minute, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY")
	if (cert == "") != (key == "") {
		log.Fatal("both TLS_CERT and TLS_KEY are required")
	}
	if cert != "" {
		err = server.ListenAndServeTLS(cert, key)
	} else {
		log.Print("HTTP agent: use only isolated local Docker network or encrypted VPN; otherwise configure TLS")
		err = server.ListenAndServe()
	}
	log.Fatal(err)
}
