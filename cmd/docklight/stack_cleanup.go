package main

func stackImageIDsFromContainers(stackName string, containers []DockerContainer) []string {
	var imageIDs []string
	for _, container := range containers {
		if container.Stack == stackName && container.ImageID != "" {
			imageIDs = append(imageIDs, container.ImageID)
		}
	}
	return uniqueStrings(imageIDs)
}

func (app *App) removeUnusedImages(imageIDs []string) (string, error) {
	if len(imageIDs) == 0 {
		return "", nil
	}
	containers, err := app.dockerContainerResources(true)
	if err != nil {
		return "", err
	}
	used := make(map[string]bool)
	for _, container := range containers {
		if container.ImageID != "" {
			used[container.ImageID] = true
		}
	}
	var removable []string
	for _, imageID := range imageIDs {
		if !used[imageID] {
			removable = append(removable, imageID)
		}
	}
	if len(removable) == 0 {
		return "", nil
	}
	args := append([]string{"image", "rm"}, removable...)
	return app.runDocker(args...)
}
