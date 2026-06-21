package main

import (
	"errors"
	"strings"
	"testing"
)

func TestParseDockerStatsAndSummarizeComposeContainers(t *testing.T) {
	stats := parseDockerStats("abc123456789|web-1|1.50%|10MiB / 1GiB\ndef456789abc|db-1|2.25%|1.5 GiB / 4 GiB\n")
	if stats["abc123456789"].CPUPercent != "1.50%" || stats["web-1"].MemoryUsage != "10MiB" {
		t.Fatalf("expected stats by id and name, got %#v", stats)
	}
	containers := enrichComposeContainerStats([]Container{{Name: "web-1"}, {Name: "db-1"}}, stats)
	cpu, memory := summarizeComposeContainerStats(containers)
	if cpu != "3.75%" || memory != "1.51 GiB" {
		t.Fatalf("expected summed cpu/memory, got cpu=%q memory=%q containers=%#v", cpu, memory, containers)
	}
}

func TestStatsHelpersHandleMissingValues(t *testing.T) {
	if got := metricValue(""); got != "-" {
		t.Fatalf("expected empty metric placeholder, got %q", got)
	}
	if _, ok := parseCPUPercent("bad"); ok {
		t.Fatal("expected bad cpu value to be rejected")
	}
	if _, ok := parseMemoryBytes("bad"); ok {
		t.Fatal("expected bad memory value to be rejected")
	}
}

func TestDockerContainerStatsFallsBackWhenBatchFails(t *testing.T) {
	app := &App{}
	var calls []string
	app.runner = func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case `docker stats --no-stream --format {{.Container}}|{{.Name}}|{{.CPUPerc}}|{{.MemUsage}} web-1 stale-1 db-1`:
			return "stale container", errors.New("stale container")
		case `docker stats --no-stream --format {{.Container}}|{{.Name}}|{{.CPUPerc}}|{{.MemUsage}} web-1`:
			return "abc123|web-1|1.00%|10MiB / 1GiB\n", nil
		case `docker stats --no-stream --format {{.Container}}|{{.Name}}|{{.CPUPerc}}|{{.MemUsage}} stale-1`:
			return "no such container", errors.New("no such container")
		case `docker stats --no-stream --format {{.Container}}|{{.Name}}|{{.CPUPerc}}|{{.MemUsage}} db-1`:
			return "def456|db-1|2.00%|20MiB / 1GiB\n", nil
		default:
			return "", nil
		}
	}

	stats, err := app.dockerContainerStats([]string{"web-1", "stale-1", "db-1"})
	if err != nil {
		t.Fatalf("expected partial stats without error, got %v", err)
	}
	if stats["web-1"].CPUPercent != "1.00%" || stats["db-1"].MemoryUsage != "20MiB" {
		t.Fatalf("expected stats for non-stale containers, got %#v", stats)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, " web-1 stale-1 db-1") || !strings.Contains(joined, " web-1") || !strings.Contains(joined, " stale-1") || !strings.Contains(joined, " db-1") {
		t.Fatalf("expected batch and individual fallback calls, got %s", joined)
	}
}

func TestDockerContainerStatsReturnsBatchErrorWhenAllFallbacksFail(t *testing.T) {
	app := &App{}
	wantErr := errors.New("batch failed")
	app.runner = func(dir, name string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "web-1 stale-1") {
			return "batch failed", wantErr
		}
		return "single failed", errors.New("single failed")
	}

	if _, err := app.dockerContainerStats([]string{"web-1", "stale-1"}); !errors.Is(err, wantErr) {
		t.Fatalf("expected original batch error, got %v", err)
	}
}
