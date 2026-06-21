package main

import (
	"fmt"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

const (
	composeProjectLabel     = "com.docker.compose.project"
	composeServiceLabel     = "com.docker.compose.service"
	composeWorkingDirLabel  = "com.docker.compose.project.working_dir"
	composeConfigFilesLabel = "com.docker.compose.project.config_files"
	selfUpdateHelperLabel   = "docklight.self-update=true"
)

type docklightSelfUpdateTarget struct {
	Project      string
	Service      string
	WorkingDir   string
	ConfigFiles  []string
	Image        string
	Mounts       []dockerMount
	DockerConfig string
	Env          []string
	HelperName   string
	AutoRemove   bool
}

type dockerMount struct {
	Source      string
	Destination string
}

func (app *App) updateDocklight(w http.ResponseWriter, r *http.Request) {
	target, err := app.docklightSelfUpdateTarget()
	if err != nil {
		app.redirectWithError(w, r, "/settings", err.Error())
		return
	}
	unlock, locked := app.tryLockStack(target.Project)
	if !locked {
		app.redirectWithError(w, r, "/settings", "Docklight update is already running")
		return
	}
	defer unlock()
	target.AutoRemove = selfUpdateAutoRemove(r)

	output, err := app.launchDocklightSelfUpdateHelper(target)
	status := "success"
	if err != nil {
		status = "failed"
	}
	if output == "" && err != nil {
		output = err.Error()
	}
	app.logActivity("docklight", "self-update helper start", status, output)
	if err != nil {
		app.redirectWithError(w, r, "/settings", output)
		return
	}
	app.redirectWithMessage(w, r, "/settings", "Docklight update helper started: "+target.HelperName)
}

func (app *App) docklightSelfUpdateTarget() (docklightSelfUpdateTarget, error) {
	containers, err := app.dockerContainerInspects(true)
	if err != nil {
		return docklightSelfUpdateTarget{}, err
	}
	if container, ok := app.currentDocklightContainer(containers); ok {
		return docklightSelfUpdateTargetFromContainer(container)
	}
	return docklightSelfUpdateTarget{}, fmt.Errorf("Docklight self-update requires the current container to be identifiable by hostname")
}

func (app *App) currentDocklightContainer(containers []dockerContainerInspect) (dockerContainerInspect, bool) {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return dockerContainerInspect{}, false
	}
	hostname = strings.TrimSpace(hostname)
	for _, container := range containers {
		id := strings.TrimPrefix(container.ID, "sha256:")
		if id == hostname || shortDockerID(id) == hostname {
			return container, true
		}
	}
	return dockerContainerInspect{}, false
}

func docklightSelfUpdateTargetFromContainer(container dockerContainerInspect) (docklightSelfUpdateTarget, error) {
	target := docklightSelfUpdateTargetFromLabels(container.Config.Labels)
	target.Image = strings.TrimSpace(container.Config.Image)
	if target.Image == "" {
		target.Image = strings.TrimSpace(container.Image)
	}
	target.DockerConfig = dockerConfigFromEnv(container.Config.Env)
	target.Env = selfUpdateHelperEnv(container.Config.Env)
	for _, mount := range container.Mounts {
		if mount.Source == "" || mount.Destination == "" {
			continue
		}
		target.Mounts = append(target.Mounts, dockerMount{Source: mount.Source, Destination: mount.Destination})
	}
	if err := target.validate(); err != nil {
		return docklightSelfUpdateTarget{}, err
	}
	target.HelperName = selfUpdateHelperName(target.Project)
	return target, nil
}

func docklightSelfUpdateTargetFromLabels(labels map[string]string) docklightSelfUpdateTarget {
	if labels == nil {
		labels = map[string]string{}
	}
	return docklightSelfUpdateTarget{
		Project:     strings.TrimSpace(labels[composeProjectLabel]),
		Service:     strings.TrimSpace(labels[composeServiceLabel]),
		WorkingDir:  strings.TrimSpace(labels[composeWorkingDirLabel]),
		ConfigFiles: splitComposeConfigFiles(labels[composeConfigFilesLabel]),
	}
}

func splitComposeConfigFiles(value string) []string {
	var files []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			files = append(files, part)
		}
	}
	return files
}

func (target docklightSelfUpdateTarget) validate() error {
	if target.Project == "" || target.Service == "" || target.WorkingDir == "" || len(target.ConfigFiles) == 0 {
		return fmt.Errorf("Docklight self-update requires Docker Compose labels")
	}
	if target.Image == "" {
		return fmt.Errorf("Docklight self-update requires the current image reference")
	}
	if !filepath.IsAbs(target.WorkingDir) {
		return fmt.Errorf("Docklight self-update working directory must be absolute: %s", target.WorkingDir)
	}
	if info, err := os.Stat(target.WorkingDir); err != nil || !info.IsDir() {
		return fmt.Errorf("Docklight self-update working directory is not available inside the container: %s", target.WorkingDir)
	}
	for _, file := range target.ConfigFiles {
		if !filepath.IsAbs(file) {
			return fmt.Errorf("Docklight self-update compose file must be absolute: %s", file)
		}
		if err := ensurePathInside(target.WorkingDir, file); err != nil {
			return fmt.Errorf("Docklight self-update compose file must be inside the working directory: %s", file)
		}
		if info, err := os.Stat(file); err != nil || info.IsDir() {
			return fmt.Errorf("Docklight self-update compose file is not available inside the container: %s", file)
		}
	}
	if _, ok := target.mountForDestination("/var/run/docker.sock"); !ok {
		return fmt.Errorf("Docklight self-update requires the Docker socket bind mount")
	}
	if mount, ok := target.mountForPath(target.WorkingDir); !ok {
		return fmt.Errorf("Docklight self-update requires the Compose project directory mounted at the same absolute path: %s", target.WorkingDir)
	} else if hostPath, ok := hostPathForContainerPath(mount, target.WorkingDir); !ok || filepath.Clean(hostPath) != filepath.Clean(target.WorkingDir) {
		return fmt.Errorf("Docklight self-update requires the Compose project directory mounted at the same absolute path: %s", target.WorkingDir)
	}
	return nil
}

func hostPathForContainerPath(mount dockerMount, pathValue string) (string, bool) {
	pathValue = pathpkg.Clean(pathValue)
	destination := pathpkg.Clean(mount.Destination)
	if pathValue != destination && !strings.HasPrefix(pathValue, destination+"/") {
		return "", false
	}
	if pathValue == destination {
		return pathpkg.Clean(mount.Source), true
	}
	rel := strings.TrimPrefix(pathValue, destination+"/")
	return pathpkg.Clean(pathpkg.Join(mount.Source, rel)), true
}

func (target docklightSelfUpdateTarget) mountForDestination(destination string) (dockerMount, bool) {
	for _, mount := range target.Mounts {
		if pathpkg.Clean(mount.Destination) == pathpkg.Clean(destination) {
			return mount, true
		}
	}
	return dockerMount{}, false
}

func (target docklightSelfUpdateTarget) mountForPath(pathValue string) (dockerMount, bool) {
	pathValue = pathpkg.Clean(pathValue)
	var best dockerMount
	bestLen := -1
	for _, mount := range target.Mounts {
		destination := pathpkg.Clean(mount.Destination)
		if pathValue != destination && !strings.HasPrefix(pathValue, destination+"/") {
			continue
		}
		if len(destination) > bestLen {
			best = mount
			bestLen = len(destination)
		}
	}
	return best, bestLen >= 0
}

func (app *App) launchDocklightSelfUpdateHelper(target docklightSelfUpdateTarget) (string, error) {
	cleanupOutput, err := app.cleanupDocklightSelfUpdateHelpers()
	if err != nil {
		return cleanupOutput, err
	}
	if runningOutput, err := app.runningDocklightSelfUpdateHelpers(target.Project); err != nil {
		return strings.TrimSpace(cleanupOutput + "\n" + runningOutput), err
	} else if strings.TrimSpace(runningOutput) != "" {
		return strings.TrimSpace(cleanupOutput + "\n" + runningOutput), fmt.Errorf("Docklight update helper already exists for project %s", target.Project)
	}
	script := target.selfUpdateHelperScript()
	socketMount, _ := target.mountForDestination("/var/run/docker.sock")
	workMount, _ := target.mountForPath(target.WorkingDir)
	hostProjectDir, _ := hostPathForContainerPath(workMount, target.WorkingDir)

	args := []string{"run", "-d"}
	if target.AutoRemove {
		args = append(args, "--rm")
	}
	args = append(args,
		"--name", target.HelperName,
		"--label", selfUpdateHelperLabel,
		"--label", "docklight.self-update.project="+target.Project,
		"-v", socketMount.Source+":/var/run/docker.sock",
		"-v", hostProjectDir+":"+target.WorkingDir+":ro",
		"-w", target.WorkingDir,
	)
	for _, value := range target.Env {
		args = append(args, "-e", value)
	}
	if target.DockerConfig != "" {
		if configMount, ok := target.mountForPath(target.DockerConfig); ok {
			args = append(args, "-e", "DOCKER_CONFIG="+target.DockerConfig, "-v", configMount.Source+":"+configMount.Destination+":ro")
		}
	}
	args = append(args, "--entrypoint", "/bin/sh", target.Image, "-c", script)
	output, err := app.runDocker(args...)
	return strings.TrimSpace(cleanupOutput + "\n" + output), err
}

func (app *App) runningDocklightSelfUpdateHelpers(project string) (string, error) {
	return app.runDocker("container", "ls", "-aq", "--filter", "label="+selfUpdateHelperLabel, "--filter", "label=docklight.self-update.project="+project)
}

func (app *App) cleanupDocklightSelfUpdateHelpers() (string, error) {
	var outputs []string
	var ids []string
	for _, status := range []string{"exited", "created", "dead"} {
		idsOutput, err := app.runDocker("container", "ls", "-aq", "--filter", "label="+selfUpdateHelperLabel, "--filter", "status="+status)
		outputs = append(outputs, strings.TrimSpace(idsOutput))
		if err != nil {
			return strings.TrimSpace(strings.Join(outputs, "\n")), err
		}
		for _, line := range strings.Split(idsOutput, "\n") {
			id := strings.TrimSpace(line)
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return strings.TrimSpace(strings.Join(outputs, "\n")), nil
	}
	removeOutput, err := app.runDocker(append([]string{"container", "rm"}, ids...)...)
	outputs = append(outputs, strings.TrimSpace(removeOutput))
	return strings.TrimSpace(strings.Join(outputs, "\n")), err
}

func (target docklightSelfUpdateTarget) selfUpdateHelperScript() string {
	cmdArgs := []string{"compose", "--project-name", target.Project, "--project-directory", target.WorkingDir}
	for _, file := range target.ConfigFiles {
		cmdArgs = append(cmdArgs, "-f", file)
	}
	pull := append(append([]string{"docker"}, cmdArgs...), "pull", target.Service)
	up := append(append([]string{"docker"}, cmdArgs...), "up", "-d", "--no-deps", "--force-recreate", target.Service)
	return "set -eu\n" + shellCommand(pull) + "\n" + shellCommand(up)
}

func shellCommand(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func dockerConfigFromEnv(values []string) string {
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if ok && key == "DOCKER_CONFIG" {
			return strings.TrimSpace(val)
		}
	}
	return "/root/.docker"
}

func selfUpdateHelperEnv(values []string) []string {
	var env []string
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if !ok || !selfUpdateAllowedEnv(key) {
			continue
		}
		env = append(env, value)
	}
	return env
}

func selfUpdateAllowedEnv(key string) bool {
	upper := strings.ToUpper(key)
	if upper == "COMPOSE_PROFILES" {
		return true
	}
	switch upper {
	case "IMAGE_TAG", "IMAGE_VERSION", "REGISTRY", "TAG", "VERSION":
		return true
	default:
		return false
	}
}

func selfUpdateHelperName(project string) string {
	name := sanitizeStackName(project)
	if name == "" {
		name = "docklight"
	}
	return "docklight-self-update-" + name
}

func selfUpdateAutoRemove(r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		return true
	}
	values, ok := r.Form["auto_remove_helper"]
	if !ok {
		return true
	}
	for _, value := range values {
		if value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "on") {
			return true
		}
	}
	return false
}
