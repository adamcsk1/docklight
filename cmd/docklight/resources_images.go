package main

import (
	"encoding/json"
	"sort"
	"strings"
)

func (app *App) dockerImages() ([]DockerImage, error) {
	ids, err := app.dockerIDs("image", "ls", "-q")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	containers, err := app.dockerContainerResources(true)
	if err != nil {
		return nil, err
	}
	used := make(map[string][]ResourceRef)
	for _, container := range containers {
		used[container.ImageID] = append(used[container.ImageID], refFromContainer(container))
	}
	output, err := app.runDocker(append([]string{"image", "inspect"}, uniqueStrings(ids)...)...)
	if err != nil {
		return nil, err
	}
	var inspected []dockerImageInspect
	if err := json.Unmarshal([]byte(output), &inspected); err != nil {
		return nil, err
	}
	images := make([]DockerImage, 0, len(inspected))
	seen := make(map[string]bool)
	for _, item := range inspected {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		name := "<none>"
		if len(item.RepoTags) > 0 {
			name = strings.Join(item.RepoTags, ", ")
		}
		users := used[item.ID]
		sortResourceRefs(users)
		protected := hasProtectedRef(users)
		images = append(images, DockerImage{ID: item.ID, ShortID: shortDockerID(item.ID), Name: name, Size: formatBytes(item.Size), Created: formatDockerTime(item.Created), UsedBy: users, Unused: len(users) == 0, Protected: protected})
	}
	sort.Slice(images, func(i, j int) bool {
		if images[i].Protected != images[j].Protected {
			return !images[i].Protected
		}
		if images[i].Unused != images[j].Unused {
			return images[i].Unused
		}
		return images[i].Name < images[j].Name
	})
	return images, nil
}

func (app *App) findImage(id string) (DockerImage, bool, error) {
	images, err := app.dockerImages()
	if err != nil {
		return DockerImage{}, false, err
	}
	for _, image := range images {
		if image.ShortID == id || strings.HasPrefix(image.ID, id) || strings.HasPrefix(strings.TrimPrefix(image.ID, "sha256:"), id) {
			return image, true, nil
		}
	}
	return DockerImage{}, false, nil
}
