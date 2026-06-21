package main

import (
	"testing"
	"time"
)

func TestFormattingAndStatusEdgeCases(t *testing.T) {
	ports := map[string][]struct{ HostIP, HostPort string }{
		"443/tcp": nil,
		"80/tcp":  {{HostIP: "0.0.0.0", HostPort: "8080"}, {HostIP: "127.0.0.1", HostPort: "8081"}},
	}
	if got := formatPorts(ports); got != "127.0.0.1:8081->80/tcp, 443/tcp, 8080->80/tcp" {
		t.Fatalf("unexpected ports %q", got)
	}
	if formatDockerTime("bad") != "bad" || formatDockerTime("0001-01-01T00:00:00Z") != "" {
		t.Fatal("unexpected docker time formatting")
	}
	if runningFor(time.Now().Add(-10*time.Second).Format(time.RFC3339Nano)) != "<1m" || runningFor("bad") != "" {
		t.Fatal("unexpected running duration formatting")
	}
	if formatBytes(1) != "1 B" || formatBytes(2048) != "2.0 KiB" || formatBytes(5*1024*1024) != "5.0 MiB" || formatBytes(2*1024*1024*1024*1024*1024) != "2.0 PiB" {
		t.Fatal("unexpected byte formatting")
	}
	if aggregateStatus([]Container{{State: "running"}, {State: "exited"}}) != "partial" || aggregateStatus([]Container{{State: "exited"}}) != "stopped" || aggregateStatus([]Container{{State: "running"}}) != "running" {
		t.Fatal("unexpected aggregate status")
	}
}
