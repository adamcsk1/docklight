package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func shortDockerID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func formatPorts(ports map[string][]struct{ HostIP, HostPort string }) string {
	var values []string
	for containerPort, bindings := range ports {
		if len(bindings) == 0 {
			values = append(values, containerPort)
			continue
		}
		for _, binding := range bindings {
			host := binding.HostPort
			if binding.HostIP != "" && binding.HostIP != "0.0.0.0" {
				host = binding.HostIP + ":" + host
			}
			values = append(values, host+"->"+containerPort)
		}
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}

func formatDockerTime(value string) string {
	if value == "" || strings.HasPrefix(value, "0001-") {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.Local().Format("2006-01-02 15:04:05")
}

func runningFor(startedAt string) string {
	if startedAt == "" || strings.HasPrefix(startedAt, "0001-") {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return ""
	}
	duration := time.Since(parsed).Round(time.Minute)
	if duration < time.Minute {
		return "<1m"
	}
	return duration.String()
}

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}
