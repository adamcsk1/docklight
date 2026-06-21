package main

import (
	"encoding/json"
	"sort"
)

func (app *App) dockerVolumes() ([]DockerVolume, error) {
	names, err := app.dockerIDs("volume", "ls", "-q")
	if err != nil || len(names) == 0 {
		return nil, err
	}
	containers, err := app.dockerContainerInspects(true)
	if err != nil {
		return nil, err
	}
	used := make(map[string][]ResourceRef)
	for _, container := range containers {
		ref := app.refFromInspect(container)
		for _, mount := range container.Mounts {
			if mount.Type == "volume" && mount.Name != "" {
				used[mount.Name] = append(used[mount.Name], ref)
			}
		}
	}
	output, err := app.runDocker(append([]string{"volume", "inspect"}, names...)...)
	if err != nil {
		return nil, err
	}
	var inspected []dockerVolumeInspect
	if err := json.Unmarshal([]byte(output), &inspected); err != nil {
		return nil, err
	}
	volumes := make([]DockerVolume, 0, len(inspected))
	for _, item := range inspected {
		users := used[item.Name]
		sortResourceRefs(users)
		volumes = append(volumes, DockerVolume{Name: item.Name, Driver: item.Driver, Mountpoint: item.Mountpoint, Created: formatDockerTime(item.CreatedAt), UsedBy: users, Unused: len(users) == 0, Protected: hasProtectedRef(users)})
	}
	sort.Slice(volumes, func(i, j int) bool {
		if volumes[i].Protected != volumes[j].Protected {
			return !volumes[i].Protected
		}
		if volumes[i].Unused != volumes[j].Unused {
			return volumes[i].Unused
		}
		return volumes[i].Name < volumes[j].Name
	})
	return volumes, nil
}

func (app *App) findVolume(name string) (DockerVolume, bool, error) {
	volumes, err := app.dockerVolumes()
	if err != nil {
		return DockerVolume{}, false, err
	}
	for _, volume := range volumes {
		if volume.Name == name {
			return volume, true, nil
		}
	}
	return DockerVolume{}, false, nil
}
