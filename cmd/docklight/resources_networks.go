package main

import (
	"encoding/json"
	"sort"
	"strings"
)

func (app *App) dockerNetworks() ([]DockerNetwork, error) {
	ids, err := app.dockerIDs("network", "ls", "-q")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	containers, err := app.dockerContainerResources(true)
	if err != nil {
		return nil, err
	}
	used := make(map[string][]ResourceRef)
	for _, container := range containers {
		ref := refFromContainer(container)
		for _, network := range container.Networks {
			used[network] = append(used[network], ref)
		}
	}
	output, err := app.runDocker(append([]string{"network", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var inspected []dockerNetworkInspect
	if err := json.Unmarshal([]byte(output), &inspected); err != nil {
		return nil, err
	}
	networks := make([]DockerNetwork, 0, len(inspected))
	for _, item := range inspected {
		users := used[item.Name]
		sortResourceRefs(users)
		protected := item.Name == "bridge" || item.Name == "host" || item.Name == "none" || hasProtectedRef(users)
		networks = append(networks, DockerNetwork{ID: item.ID, ShortID: shortDockerID(item.ID), Name: item.Name, Driver: item.Driver, Scope: item.Scope, Internal: item.Internal, Attachable: item.Attachable, UsedBy: users, Unused: len(users) == 0, Protected: protected})
	}
	sort.Slice(networks, func(i, j int) bool {
		if networks[i].Protected != networks[j].Protected {
			return !networks[i].Protected
		}
		if networks[i].Unused != networks[j].Unused {
			return networks[i].Unused
		}
		return networks[i].Name < networks[j].Name
	})
	return networks, nil
}

func (app *App) findNetwork(id string) (DockerNetwork, bool, error) {
	networks, err := app.dockerNetworks()
	if err != nil {
		return DockerNetwork{}, false, err
	}
	for _, network := range networks {
		if network.ShortID == id || network.Name == id || strings.HasPrefix(network.ID, id) {
			return network, true, nil
		}
	}
	return DockerNetwork{}, false, nil
}
