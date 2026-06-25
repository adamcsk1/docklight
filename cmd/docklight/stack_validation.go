package main

import (
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func validateStackName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return "", errors.New("stack name is required")
	}
	if strings.ContainsAny(name, `/\\`) || strings.Contains(name, "..") {
		return "", errors.New("stack name must be a folder name, not a path")
	}
	sanitized := sanitizeStackName(name)
	if sanitized == "" || sanitized != name {
		return "", fmt.Errorf("stack name must only contain lowercase letters, numbers, dash, or underscore; suggested: %s", sanitized)
	}
	return sanitized, nil
}

func parseBindFolders(input string) ([]string, error) {
	var folders []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(input, "\n") {
		raw := strings.TrimSpace(line)
		if strings.ContainsAny(raw, `:\`) {
			return nil, fmt.Errorf("bind folder must be relative and stay inside the stack folder: %s", line)
		}
		folder := path.Clean(raw)
		if folder == "." || folder == "" {
			continue
		}
		if strings.HasPrefix(folder, "/") || folder == ".." || strings.HasPrefix(folder, "../") || strings.Contains(folder, "/../") {
			return nil, fmt.Errorf("bind folder must be relative and stay inside the stack folder: %s", line)
		}
		if !seen[folder] {
			seen[folder] = true
			folders = append(folders, folder)
		}
	}
	return folders, nil
}

func (app *App) validateComposeDefinition(projectName, stackDir, composeYAML, envFile string, allowExternalBinds, allowDockerSocketMounts bool) error {
	if !composeNeedsInterpolation(composeYAML, envFile) {
		return validateComposeYAMLWithExternalBinds(composeYAML, allowExternalBinds, allowDockerSocketMounts)
	}
	if err := app.ensureStackPathSafe(stackDir); err != nil {
		return err
	}
	composeFile, err := os.CreateTemp(stackDir, ".docklight-validate-*.yml")
	if err != nil {
		return err
	}
	composePath := composeFile.Name()
	defer os.Remove(composePath)
	if _, err := composeFile.WriteString(composeYAML + "\n"); err != nil {
		_ = composeFile.Close()
		return err
	}
	if err := composeFile.Close(); err != nil {
		return err
	}

	args := []string{"compose", "--project-name", projectName, "--project-directory", stackDir}
	var envPath string
	if strings.TrimSpace(envFile) != "" {
		env, err := os.CreateTemp(stackDir, ".docklight-validate-*.env")
		if err != nil {
			return err
		}
		envPath = env.Name()
		defer os.Remove(envPath)
		if _, err := env.WriteString(envFile + "\n"); err != nil {
			_ = env.Close()
			return err
		}
		if err := env.Close(); err != nil {
			return err
		}
		args = append(args, "--env-file", envPath)
	}
	args = append(args, "-f", composePath, "config")
	output, err := app.runCommand(stackDir, "docker", args...)
	if err != nil {
		message := strings.TrimSpace(output)
		if message == "" {
			message = err.Error()
		}
		return errors.New("compose config failed: " + message)
	}
	resolved := strings.TrimSpace(output)
	if resolved == "" {
		return validateComposeYAMLWithExternalBinds(composeYAML, allowExternalBinds, allowDockerSocketMounts)
	}
	return validateResolvedComposeYAML(resolved, stackDir, allowExternalBinds, allowDockerSocketMounts)
}

func composeNeedsInterpolation(composeYAML, envFile string) bool {
	return strings.TrimSpace(envFile) != "" || strings.Contains(composeYAML, "${")
}

func validateComposeYAML(input string) error {
	return validateComposeYAMLWithExternalBinds(input, false, false)
}

func validateComposeYAMLWithExternalBinds(input string, allowExternalBinds, allowDockerSocketMounts bool) error {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(input), &root); err != nil {
		return err
	}
	return validateComposeNode(&root, "", allowExternalBinds, allowDockerSocketMounts)
}

func validateResolvedComposeYAML(input, stackDir string, allowExternalBinds, allowDockerSocketMounts bool) error {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(input), &root); err != nil {
		return err
	}
	return validateComposeNode(&root, stackDir, allowExternalBinds, allowDockerSocketMounts)
}

func validateComposeNode(node *yaml.Node, stackDir string, allowExternalBinds, allowDockerSocketMounts bool) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode && !allowDockerSocketMounts && strings.Contains(node.Value, "/var/run/docker.sock") {
		return errors.New("compose yaml contains blocked docker socket mount")
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := strings.ToLower(strings.TrimSpace(node.Content[i].Value))
			value := node.Content[i+1]
			switch key {
			case "privileged":
				if yamlBool(value) {
					return errors.New("compose yaml contains blocked host-level option: privileged")
				}
			case "network_mode", "pid", "ipc", "cgroup", "userns_mode":
				if strings.EqualFold(strings.TrimSpace(value.Value), "host") {
					return fmt.Errorf("compose yaml contains blocked host-level option: %s", key)
				}
			case "cap_add", "devices", "security_opt":
				return fmt.Errorf("compose yaml contains blocked host-level option: %s", key)
			case "volumes":
				if err := validateComposeVolumes(value, stackDir, allowExternalBinds, allowDockerSocketMounts); err != nil {
					return err
				}
			}
			if err := validateComposeNode(value, stackDir, allowExternalBinds, allowDockerSocketMounts); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := validateComposeNode(child, stackDir, allowExternalBinds, allowDockerSocketMounts); err != nil {
			return err
		}
	}
	return nil
}

func validateComposeVolumes(node *yaml.Node, stackDir string, allowExternalBinds, allowDockerSocketMounts bool) error {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			value := strings.Trim(item.Value, `"'`)
			if source, _, ok := splitVolumeSpec(value); ok {
				if bindSourceExposesDockerSocket(source, allowDockerSocketMounts) {
					return fmt.Errorf("compose yaml contains blocked docker socket mount: %s", source)
				}
				if dockerSocketMountAllowed(source, allowDockerSocketMounts) {
					continue
				}
			}
			if looksLikeUnsafeHostBind(value) && !allowExternalBinds && !composeBindSourceInsideStack(value, stackDir) {
				source, _, _ := splitVolumeSpec(value)
				return fmt.Errorf("compose yaml contains unsafe host bind mount: %s", source)
			}
		case yaml.MappingNode:
			typeNode := mappingValue(item, "type")
			if typeNode != nil && strings.EqualFold(typeNode.Value, "bind") {
				sourceNode := mappingValue(item, "source")
				if sourceNode == nil {
					sourceNode = mappingValue(item, "src")
				}
				if sourceNode == nil {
					return errors.New("compose yaml contains unsafe host bind mount")
				}
				if bindSourceExposesDockerSocket(sourceNode.Value, allowDockerSocketMounts) {
					return fmt.Errorf("compose yaml contains blocked docker socket mount: %s", sourceNode.Value)
				}
				if dockerSocketMountAllowed(sourceNode.Value, allowDockerSocketMounts) {
					continue
				}
				if !allowExternalBinds && !pathInsideOrEqual(stackDir, sourceNode.Value) {
					return fmt.Errorf("compose yaml contains unsafe host bind mount: %s", sourceNode.Value)
				}
			}
		}
	}
	return nil
}

func dockerSocketMountAllowed(source string, allowExactSocket bool) bool {
	if !allowExactSocket {
		return false
	}
	cleaned := filepath.ToSlash(path.Clean(filepath.ToSlash(strings.TrimSpace(source))))
	return cleaned == "/var/run/docker.sock" || cleaned == "/run/docker.sock"
}

func bindSourceExposesDockerSocket(source string, allowExactSocket bool) bool {
	source = strings.TrimSpace(source)
	if source == "" || strings.HasPrefix(source, "${") {
		return false
	}
	cleaned := filepath.ToSlash(path.Clean(filepath.ToSlash(source)))
	for _, socket := range []string{"/var/run/docker.sock", "/run/docker.sock"} {
		if cleaned == socket {
			return !allowExactSocket
		}
		if cleaned == path.Dir(socket) || strings.HasPrefix(socket, cleaned+"/") {
			return true
		}
	}
	return false
}

func composeBindSourceInsideStack(value, stackDir string) bool {
	source, _, ok := splitVolumeSpec(value)
	return ok && pathInsideOrEqual(stackDir, source)
}

func pathInsideOrEqual(rootPath, targetPath string) bool {
	if rootPath == "" || targetPath == "" {
		return false
	}
	root, err := filepath.Abs(rootPath)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(filepath.FromSlash(targetPath))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || !pathEscapesRoot(rel)
}

func yamlBool(node *yaml.Node) bool {
	return strings.EqualFold(strings.TrimSpace(node.Value), "true") || node.Tag == "!!bool" && strings.EqualFold(node.Value, "true")
}

func looksLikeUnsafeHostBind(value string) bool {
	if value == "" || strings.HasPrefix(value, "type=") {
		return false
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 {
		return false
	}
	if len(parts) > 2 && len(parts[0]) == 1 && strings.Contains(value, `\`) {
		return true
	}
	source := strings.TrimSpace(parts[0])
	if source == "" || strings.HasPrefix(source, "${") {
		return true
	}
	cleaned := path.Clean(source)
	return strings.HasPrefix(source, "/") || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") || strings.ContainsAny(source, `\`)
}
