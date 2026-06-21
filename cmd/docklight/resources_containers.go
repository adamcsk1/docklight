package main

import (
	"sort"
	"strings"
)

func (app *App) dockerContainerResources(all bool) ([]DockerContainer, error) {
	inspected, err := app.dockerContainerInspects(all)
	if err != nil {
		return nil, err
	}
	knownStacks, err := app.stackNameSet()
	if err != nil {
		return nil, err
	}
	containers := make([]DockerContainer, 0, len(inspected))
	for _, item := range inspected {
		labels := item.Config.Labels
		project := labels["com.docker.compose.project"]
		service := labels["com.docker.compose.service"]
		stack := "standalone"
		if project != "" {
			stack = project
			if !knownStacks[project] {
				stack = "external:" + project
			}
		}
		health := "none"
		if item.State.Health != nil && item.State.Health.Status != "" {
			health = item.State.Health.Status
		}
		protected := app.isDocklightContainerInspect(item)
		containers = append(containers, DockerContainer{
			ID:         shortDockerID(item.ID),
			Name:       strings.TrimPrefix(item.Name, "/"),
			Image:      item.Config.Image,
			ImageID:    item.Image,
			State:      item.State.Status,
			Health:     health,
			Started:    formatDockerTime(item.State.StartedAt),
			RunningFor: runningFor(item.State.StartedAt),
			Stack:      stack,
			Service:    service,
			Ports:      formatPorts(item.NetworkSettings.Ports),
			Networks:   sortedMapKeys(item.NetworkSettings.Networks),
			Protected:  protected,
		})
	}
	sort.Slice(containers, func(i, j int) bool {
		if containers[i].Protected != containers[j].Protected {
			return !containers[i].Protected
		}
		return containers[i].Name < containers[j].Name
	})
	return containers, nil
}
