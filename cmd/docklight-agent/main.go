package main

import (
	"crypto/tls"
	"log"
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

func main() {
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
