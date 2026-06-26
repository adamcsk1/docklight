package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const stackManagedConfigFile = ".docklight.config.yml"

type StackManagedConfig struct {
	Folders  []string                 `yaml:"folders,omitempty"`
	Files    []StackManagedConfigFile `yaml:"files,omitempty"`
	Settings StackManagedSettings     `yaml:"settings,omitempty"`
}

type StackManagedConfigFile struct {
	Path string `yaml:"path"`
}

type StackManagedSettings struct {
	ExternalBindMountsAllowed bool `yaml:"external_bind_mounts_allowed,omitempty"`
	DockerSocketMountsAllowed bool `yaml:"docker_socket_mounts_allowed,omitempty"`
	HostPIDAllowed            bool `yaml:"host_pid_allowed,omitempty"`
}

type managedFileWrite struct {
	Path    string
	Content string
}

func (app *App) loadManagedStackConfig(stackDir string) (StackManagedConfig, error) {
	var cfg StackManagedConfig
	path := filepath.Join(stackDir, stackManagedConfigFile)
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return cfg, err
	}
	folders, err := parseBindFolders(strings.Join(cfg.Folders, "\n"))
	if err != nil {
		return cfg, err
	}
	files, err := parseManagedFilePaths(configFilePaths(cfg.Files))
	if err != nil {
		return cfg, err
	}
	cfg.Folders = folders
	cfg.Files = make([]StackManagedConfigFile, 0, len(files))
	for _, file := range files {
		cfg.Files = append(cfg.Files, StackManagedConfigFile{Path: file})
	}
	return cfg, nil
}

func (app *App) loadManagedStackConfigOrDefault(stackDir string) StackManagedConfig {
	cfg, err := app.loadManagedStackConfig(stackDir)
	if err != nil {
		if app.logger != nil {
			app.logger.Warn("ignoring invalid stack managed config", "path", filepath.Join(stackDir, stackManagedConfigFile), "error", err)
		}
		return StackManagedConfig{}
	}
	return cfg
}

func writeManagedStackConfig(stackDir string, folders []string, files []managedFileWrite, settings StackManagedSettings) error {
	cfg := StackManagedConfig{Folders: folders, Settings: settings}
	for _, file := range files {
		cfg.Files = append(cfg.Files, StackManagedConfigFile{Path: file.Path})
	}
	return writeStackManagedConfig(stackDir, cfg)
}

func writeStackManagedConfig(stackDir string, cfg StackManagedConfig) error {
	content, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stackDir, stackManagedConfigFile), content, 0o644)
}

func stackManagedSettingsFromForm(form StackForm) StackManagedSettings {
	return StackManagedSettings{
		ExternalBindMountsAllowed: form.ExternalBindMountsAllowed,
		DockerSocketMountsAllowed: form.DockerSocketMountsAllowed,
		HostPIDAllowed:            form.HostPIDAllowed,
	}
}

func applyManagedSettingsToStack(stack *Stack, settings StackManagedSettings) {
	stack.ExternalBindMountsAllowed = settings.ExternalBindMountsAllowed
	stack.DockerSocketMountsAllowed = settings.DockerSocketMountsAllowed
	stack.HostPIDAllowed = settings.HostPIDAllowed
}

func managedFoldersText(folders []string) string {
	return strings.Join(folders, "\n")
}

func (app *App) managedFileForms(stackDir string, files []StackManagedConfigFile) ([]ManagedFileForm, error) {
	forms := make([]ManagedFileForm, 0, len(files))
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(stackDir, filepath.FromSlash(file.Path)))
		if errors.Is(err, os.ErrNotExist) {
			forms = append(forms, ManagedFileForm{Path: file.Path})
			continue
		}
		if err != nil {
			return nil, err
		}
		forms = append(forms, ManagedFileForm{Path: file.Path, Content: string(content)})
	}
	return forms, nil
}

func parseManagedFiles(files []ManagedFileForm) ([]managedFileWrite, error) {
	var result []managedFileWrite
	seen := make(map[string]bool)
	for _, file := range files {
		if file.Path == "" && strings.TrimSpace(file.Content) == "" {
			continue
		}
		filePath, err := parseManagedFilePath(file.Path)
		if err != nil {
			return nil, err
		}
		if seen[filePath] {
			continue
		}
		seen[filePath] = true
		result = append(result, managedFileWrite{Path: filePath, Content: file.Content})
	}
	return result, nil
}

func parseManagedFilePaths(paths []string) ([]string, error) {
	var files []string
	seen := make(map[string]bool)
	for _, rawPath := range paths {
		filePath, err := parseManagedFilePath(rawPath)
		if err != nil {
			return nil, err
		}
		if !seen[filePath] {
			seen[filePath] = true
			files = append(files, filePath)
		}
	}
	return files, nil
}

func parseManagedFilePath(rawPath string) (string, error) {
	filePath, err := cleanStackRelativePath(rawPath, "managed file")
	if err != nil {
		return "", err
	}
	if filePath == stackManagedConfigFile {
		return "", fmt.Errorf("managed file cannot be %s", stackManagedConfigFile)
	}
	if strings.HasSuffix(strings.TrimSpace(rawPath), "/") {
		return "", fmt.Errorf("managed file must include a filename: %s", rawPath)
	}
	return filePath, nil
}

func cleanStackRelativePath(rawValue, label string) (string, error) {
	raw := strings.TrimSpace(rawValue)
	if raw == "" {
		return "", fmt.Errorf("%s path is required", label)
	}
	if strings.ContainsAny(raw, `:\`) {
		return "", fmt.Errorf("%s must be relative and stay inside the stack folder: %s", label, rawValue)
	}
	cleaned := path.Clean(raw)
	if cleaned == "." || strings.HasPrefix(cleaned, "/") || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("%s must be relative and stay inside the stack folder: %s", label, rawValue)
	}
	return cleaned, nil
}

func writeManagedFiles(stackDir string, folders []string, files []managedFileWrite) error {
	if err := validateManagedFileParents(stackDir, folders, files); err != nil {
		return err
	}
	for _, file := range files {
		filePath := filepath.Join(stackDir, filepath.FromSlash(file.Path))
		if err := ensurePathInside(stackDir, filePath); err != nil {
			return err
		}
		if err := os.WriteFile(filePath, []byte(file.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func validateManagedFileParents(stackDir string, folders []string, files []managedFileWrite) error {
	allowedParents := map[string]bool{".": true}
	for _, folder := range folders {
		allowedParents[folder] = true
	}
	for _, file := range files {
		parent := path.Dir(file.Path)
		if !allowedParents[parent] {
			parentPath := filepath.Join(stackDir, filepath.FromSlash(parent))
			info, err := os.Stat(parentPath)
			if err != nil || !info.IsDir() {
				return fmt.Errorf("managed file parent folder does not exist: %s", parent)
			}
		}
		if err := ensurePathInside(stackDir, filepath.Join(stackDir, filepath.FromSlash(file.Path))); err != nil {
			return err
		}
	}
	return nil
}

func configFilePaths(files []StackManagedConfigFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
}
