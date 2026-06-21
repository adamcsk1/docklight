package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type containerStat struct {
	CPUPercent  string
	MemoryUsage string
}

func (app *App) dockerContainerStats(containers []string) (map[string]containerStat, error) {
	containers = uniqueNonEmptyStrings(containers)
	if len(containers) == 0 {
		return nil, nil
	}
	args := append([]string{"stats", "--no-stream", "--format", "{{.Container}}|{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}"}, containers...)
	output, err := app.runDocker(args...)
	if err != nil {
		return app.dockerContainerStatsIndividually(containers, err)
	}
	return parseDockerStats(output), nil
}

func (app *App) dockerContainerStatsIndividually(containers []string, batchErr error) (map[string]containerStat, error) {
	if len(containers) <= 1 {
		return nil, batchErr
	}
	stats := make(map[string]containerStat)
	for _, container := range containers {
		args := []string{"stats", "--no-stream", "--format", "{{.Container}}|{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}", container}
		output, err := app.runDocker(args...)
		if err != nil {
			continue
		}
		for key, value := range parseDockerStats(output) {
			stats[key] = value
		}
	}
	if len(stats) == 0 {
		return nil, batchErr
	}
	return stats, nil
}

func parseDockerStats(output string) map[string]containerStat {
	stats := make(map[string]containerStat)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		stat := containerStat{CPUPercent: strings.TrimSpace(parts[2]), MemoryUsage: currentMemoryUsage(parts[3])}
		if id != "" {
			stats[id] = stat
			stats[shortDockerID(id)] = stat
			stats[strings.TrimPrefix(id, "sha256:")] = stat
		}
		if name != "" {
			stats[name] = stat
			stats[strings.TrimPrefix(name, "/")] = stat
		}
	}
	return stats
}

func currentMemoryUsage(value string) string {
	current, _, found := strings.Cut(strings.TrimSpace(value), "/")
	if found {
		return strings.TrimSpace(current)
	}
	return strings.TrimSpace(value)
}

func enrichComposeContainerStats(containers []Container, stats map[string]containerStat) []Container {
	for i := range containers {
		if stat, ok := stats[containers[i].Name]; ok {
			containers[i].CPUPercent = stat.CPUPercent
			containers[i].MemoryUsage = stat.MemoryUsage
		}
	}
	return containers
}

func enrichDockerContainerStats(containers []DockerContainer, stats map[string]containerStat) []DockerContainer {
	for i := range containers {
		if stat, ok := stats[containers[i].ID]; ok {
			containers[i].CPUPercent = stat.CPUPercent
			containers[i].MemoryUsage = stat.MemoryUsage
			continue
		}
		if stat, ok := stats[containers[i].Name]; ok {
			containers[i].CPUPercent = stat.CPUPercent
			containers[i].MemoryUsage = stat.MemoryUsage
		}
	}
	return containers
}

func runningComposeContainerNames(containers []Container) []string {
	names := make([]string, 0, len(containers))
	for _, container := range containers {
		if container.Name != "" && strings.EqualFold(container.State, "running") {
			names = append(names, container.Name)
		}
	}
	return names
}

func usageContainerNames(containers []Container) string {
	return strings.Join(runningComposeContainerNames(containers), " ")
}

func runningDockerContainerIDs(containers []DockerContainer) []string {
	ids := make([]string, 0, len(containers))
	for _, container := range containers {
		if container.ID != "" && strings.EqualFold(container.State, "running") {
			ids = append(ids, container.ID)
		}
	}
	return ids
}

func summarizeComposeContainerStats(containers []Container) (string, string) {
	var cpuTotal float64
	var memoryBytes float64
	var cpuCount, memoryCount int
	for _, container := range containers {
		if cpu, ok := parseCPUPercent(container.CPUPercent); ok {
			cpuTotal += cpu
			cpuCount++
		}
		if bytes, ok := parseMemoryBytes(container.MemoryUsage); ok {
			memoryBytes += bytes
			memoryCount++
		}
	}
	cpu := ""
	if cpuCount > 0 {
		cpu = formatCPUPercent(cpuTotal)
	}
	memory := ""
	if memoryCount > 0 {
		memory = formatMemoryBytes(memoryBytes)
	}
	return cpu, memory
}

func summarizeContainerStatsByNames(containers []string, stats map[string]containerStat) (string, string) {
	var rows []Container
	for _, container := range containers {
		stat, ok := stats[container]
		if !ok {
			continue
		}
		rows = append(rows, Container{CPUPercent: stat.CPUPercent, MemoryUsage: stat.MemoryUsage})
	}
	return summarizeComposeContainerStats(rows)
}

func metricValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]bool)
	var unique []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}

func parseCPUPercent(value string) (float64, bool) {
	value = strings.TrimSpace(strings.TrimSuffix(value, "%"))
	if value == "" {
		return 0, false
	}
	cpu, err := strconv.ParseFloat(value, 64)
	return cpu, err == nil
}

func formatCPUPercent(value float64) string {
	if math.Abs(value) < 0.005 {
		return "0.00%"
	}
	return fmt.Sprintf("%.2f%%", value)
}

func parseMemoryBytes(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	fields := strings.Fields(value)
	if len(fields) == 2 {
		amount, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0, false
		}
		multiplier, ok := memoryUnitMultiplier(fields[1])
		return amount * multiplier, ok
	}
	for i, r := range value {
		if (r < '0' || r > '9') && r != '.' {
			amount, err := strconv.ParseFloat(value[:i], 64)
			if err != nil {
				return 0, false
			}
			multiplier, ok := memoryUnitMultiplier(value[i:])
			return amount * multiplier, ok
		}
	}
	amount, err := strconv.ParseFloat(value, 64)
	return amount, err == nil
}

func memoryUnitMultiplier(unit string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "b", "byte", "bytes":
		return 1, true
	case "kb":
		return 1000, true
	case "kib":
		return 1024, true
	case "mb":
		return 1000 * 1000, true
	case "mib":
		return 1024 * 1024, true
	case "gb":
		return 1000 * 1000 * 1000, true
	case "gib":
		return 1024 * 1024 * 1024, true
	case "tb":
		return 1000 * 1000 * 1000 * 1000, true
	case "tib":
		return 1024 * 1024 * 1024 * 1024, true
	default:
		return 0, false
	}
}

func formatMemoryBytes(bytes float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	unit := 0
	for bytes >= 1024 && unit < len(units)-1 {
		bytes /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%.0f %s", bytes, units[unit])
	}
	if bytes >= 10 {
		return fmt.Sprintf("%.1f %s", bytes, units[unit])
	}
	return fmt.Sprintf("%.2f %s", bytes, units[unit])
}
