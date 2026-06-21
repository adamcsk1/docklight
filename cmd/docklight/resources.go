package main

import (
	"os"
	"sort"
	"strings"
)

type ResourceRef struct {
	Name      string
	ID        string
	Stack     string
	Service   string
	State     string
	Protected bool
}

type DockerContainer struct {
	ID          string
	Name        string
	Image       string
	ImageID     string
	State       string
	Health      string
	Started     string
	RunningFor  string
	Stack       string
	Service     string
	CPUPercent  string
	MemoryUsage string
	Ports       string
	Networks    []string
	Protected   bool
}

type DockerImage struct {
	ID        string
	ShortID   string
	Name      string
	Size      string
	Created   string
	UsedBy    []ResourceRef
	Unused    bool
	Protected bool
}

type DockerVolume struct {
	Name       string
	Driver     string
	Mountpoint string
	Created    string
	UsedBy     []ResourceRef
	Unused     bool
	Protected  bool
}

type DockerNetwork struct {
	ID         string
	ShortID    string
	Name       string
	Driver     string
	Scope      string
	Internal   bool
	Attachable bool
	UsedBy     []ResourceRef
	Unused     bool
	Protected  bool
}

func refFromContainer(container DockerContainer) ResourceRef {
	return ResourceRef{Name: container.Name, ID: container.ID, Stack: container.Stack, Service: container.Service, State: container.State, Protected: container.Protected}
}

func (app *App) refFromInspect(container dockerContainerInspect) ResourceRef {
	labels := container.Config.Labels
	stack := labels["com.docker.compose.project"]
	if stack == "" {
		stack = "standalone"
	}
	return ResourceRef{Name: strings.TrimPrefix(container.Name, "/"), ID: shortDockerID(container.ID), Stack: stack, Service: labels["com.docker.compose.service"], State: container.State.Status, Protected: app.isDocklightContainerInspect(container)}
}

func (app *App) isDocklightContainerInspect(container dockerContainerInspect) bool {
	name := strings.TrimPrefix(container.Name, "/")
	labels := container.Config.Labels
	if strings.EqualFold(name, "docklight") || strings.EqualFold(labels["com.docker.compose.service"], "docklight") || strings.EqualFold(labels["com.docker.compose.project"], "docklight") {
		return true
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return false
	}
	id := strings.TrimPrefix(container.ID, "sha256:")
	hostname = strings.TrimSpace(hostname)
	return strings.HasPrefix(id, hostname) || shortDockerID(id) == hostname
}

func hasProtectedRef(refs []ResourceRef) bool {
	for _, ref := range refs {
		if ref.Protected {
			return true
		}
	}
	return false
}

func sortResourceRefs(refs []ResourceRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Protected != refs[j].Protected {
			return !refs[i].Protected
		}
		if refs[i].Stack != refs[j].Stack {
			return refs[i].Stack < refs[j].Stack
		}
		return refs[i].Name < refs[j].Name
	})
}
