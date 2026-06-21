package main

import (
	"encoding/json"
	"sort"
	"strings"
)

func parseComposePSJSON(output string) ([]Container, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}

	var rows []composePSContainer
	if err := json.Unmarshal([]byte(output), &rows); err == nil {
		return composePSRowsToContainers(rows), nil
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row composePSContainer
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return composePSRowsToContainers(rows), nil
}

func composePSRowsToContainers(rows []composePSContainer) []Container {
	containers := make([]Container, 0, len(rows))
	for _, row := range rows {
		containers = append(containers, Container{Name: row.Name, Service: row.Service, State: row.State, Status: row.Status, Image: row.Image})
	}
	return containers
}

func parseComposePSLines(output string) []Container {
	var containers []Container
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		for len(parts) < 5 {
			parts = append(parts, "")
		}
		containers = append(containers, Container{Name: parts[0], Service: parts[1], State: parts[2], Status: parts[3], Image: parts[4]})
	}
	return containers
}

func sortComposeContainers(containers []Container) []Container {
	sort.Slice(containers, func(i, j int) bool {
		if isDocklightComposeContainer(containers[i]) != isDocklightComposeContainer(containers[j]) {
			return !isDocklightComposeContainer(containers[i])
		}
		return containers[i].Name < containers[j].Name
	})
	return containers
}

func isDocklightComposeContainer(container Container) bool {
	return strings.EqualFold(container.Name, "docklight") || strings.EqualFold(container.Service, "docklight")
}
