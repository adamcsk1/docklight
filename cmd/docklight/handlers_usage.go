package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

type usageMetric struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

const maxUsageTargets = 200

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (app *App) containerUsage(w http.ResponseWriter, r *http.Request) {
	groups := usageTargetGroups(r.URL.Query()["target"])
	containers := usageGroupContainers(groups)
	stats, err := app.dockerContainerStats(containers)
	if err != nil {
		stats = nil
	}
	metrics := make(map[string]usageMetric, len(groups))
	for key, containers := range groups {
		cpu, memory := summarizeContainerStatsByNames(containers, stats)
		metrics[key] = usageMetric{CPU: cpu, Memory: memory}
	}
	writeJSON(w, metrics)
}

func usageTargetGroups(values []string) map[string][]string {
	groups := make(map[string][]string)
	seen := make(map[string]bool)
	count := 0
	for _, value := range values {
		key, container, ok := strings.Cut(value, "|")
		key = strings.TrimSpace(key)
		container = strings.TrimSpace(container)
		if !ok || key == "" || container == "" {
			continue
		}
		pair := key + "|" + container
		if seen[pair] {
			continue
		}
		if count >= maxUsageTargets {
			break
		}
		seen[pair] = true
		groups[key] = append(groups[key], container)
		count++
	}
	return groups
}

func usageGroupContainers(groups map[string][]string) []string {
	var containers []string
	for _, group := range groups {
		containers = append(containers, group...)
	}
	return containers
}
