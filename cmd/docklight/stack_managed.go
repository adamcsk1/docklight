package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const stackManagedConfigFile = ".docklight.config.yml"

func errWindowsChownUnsupported(label, targetPath string) error {
	return fmt.Errorf("%s chown is not supported on Windows: %s", label, targetPath)
}

type StackManagedConfig struct {
	Folders  []StackManagedConfigFolder `yaml:"folders,omitempty"`
	Files    []StackManagedConfigFile   `yaml:"files,omitempty"`
	Settings StackManagedSettings       `yaml:"settings,omitempty"`
}

type StackManagedConfigFolder struct {
	Path  string `yaml:"path"`
	Chmod string `yaml:"chmod,omitempty"`
	Chown string `yaml:"chown,omitempty"`
}

type StackManagedConfigFile struct {
	Path  string `yaml:"path"`
	Chmod string `yaml:"chmod,omitempty"`
	Chown string `yaml:"chown,omitempty"`
}

type StackManagedSettings struct {
	ExternalBindMountsAllowed  bool `yaml:"external_bind_mounts_allowed,omitempty"`
	DockerSocketMountsAllowed  bool `yaml:"docker_socket_mounts_allowed,omitempty"`
	USBDeviceBindMountsAllowed bool `yaml:"usb_device_bind_mounts_allowed,omitempty"`
	DBusBindMountAllowed       bool `yaml:"dbus_bind_mount_allowed,omitempty"`
	HostPIDAllowed             bool `yaml:"host_pid_allowed,omitempty"`
	PrivilegedAllowed          bool `yaml:"privileged_allowed,omitempty"`
	SecurityOptAllowed         bool `yaml:"security_opt_allowed,omitempty"`
}

type managedFileWrite struct {
	Path    string
	Content string
	Chmod   string
	Chown   string
}

func (app *App) loadManagedStackConfig(stackDir string) (StackManagedConfig, error) {
	var cfg StackManagedConfig
	path := filepath.Join(stackDir, stackManagedConfigFile)
	if err := ensureNoSymlinkInStackPath(stackDir, stackManagedConfigFile); err != nil {
		return cfg, err
	}
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
	folders, err := parseManagedFolders(cfg.Folders)
	if err != nil {
		return cfg, err
	}
	files, err := parseManagedConfigFiles(cfg.Files)
	if err != nil {
		return cfg, err
	}
	cfg.Folders = folders
	cfg.Files = files
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

func writeManagedStackConfig(stackDir string, folders []StackManagedConfigFolder, files []managedFileWrite, settings StackManagedSettings) error {
	cfg := StackManagedConfig{Folders: folders, Settings: settings}
	for _, file := range files {
		cfg.Files = append(cfg.Files, StackManagedConfigFile{Path: file.Path, Chmod: file.Chmod, Chown: file.Chown})
	}
	return writeStackManagedConfig(stackDir, cfg)
}

func writeStackManagedConfig(stackDir string, cfg StackManagedConfig) error {
	content, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := ensureNoSymlinkInStackPath(stackDir, stackManagedConfigFile); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stackDir, stackManagedConfigFile), content, 0o644)
}

func stackManagedSettingsFromForm(form StackForm) StackManagedSettings {
	return StackManagedSettings{
		ExternalBindMountsAllowed:  form.ExternalBindMountsAllowed,
		DockerSocketMountsAllowed:  form.DockerSocketMountsAllowed,
		USBDeviceBindMountsAllowed: form.USBDeviceBindMountsAllowed,
		DBusBindMountAllowed:       form.DBusBindMountAllowed,
		HostPIDAllowed:             form.HostPIDAllowed,
		PrivilegedAllowed:          form.PrivilegedAllowed,
		SecurityOptAllowed:         form.SecurityOptAllowed,
	}
}

func applyManagedSettingsToStack(stack *Stack, settings StackManagedSettings) {
	stack.ExternalBindMountsAllowed = settings.ExternalBindMountsAllowed
	stack.DockerSocketMountsAllowed = settings.DockerSocketMountsAllowed
	stack.USBDeviceBindMountsAllowed = settings.USBDeviceBindMountsAllowed
	stack.DBusBindMountAllowed = settings.DBusBindMountAllowed
	stack.HostPIDAllowed = settings.HostPIDAllowed
	stack.PrivilegedAllowed = settings.PrivilegedAllowed
	stack.SecurityOptAllowed = settings.SecurityOptAllowed
}

func managedFolderForms(folders []StackManagedConfigFolder) []ManagedFolderForm {
	forms := make([]ManagedFolderForm, 0, len(folders))
	for _, folder := range folders {
		forms = append(forms, ManagedFolderForm{Path: folder.Path, Chmod: folder.Chmod, Chown: folder.Chown})
	}
	return forms
}

func (app *App) managedFileForms(stackDir string, files []StackManagedConfigFile) ([]ManagedFileForm, error) {
	forms := make([]ManagedFileForm, 0, len(files))
	for _, file := range files {
		filePath := filepath.Join(stackDir, filepath.FromSlash(file.Path))
		if err := ensurePathInside(stackDir, filePath); err != nil {
			return nil, err
		}
		if err := ensureNoSymlinkInStackPath(stackDir, file.Path); err != nil {
			return nil, err
		}
		if err := ensureResolvedPathInside(stackDir, filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		content, err := os.ReadFile(filePath)
		if errors.Is(err, os.ErrNotExist) {
			forms = append(forms, ManagedFileForm{Path: file.Path, Chmod: file.Chmod, Chown: file.Chown})
			continue
		}
		if err != nil {
			return nil, err
		}
		forms = append(forms, ManagedFileForm{Path: file.Path, Content: string(content), Chmod: file.Chmod, Chown: file.Chown})
	}
	return forms, nil
}

func parseManagedFiles(files []ManagedFileForm) ([]managedFileWrite, error) {
	var result []managedFileWrite
	seen := make(map[string]bool)
	for _, file := range files {
		if file.Path == "" && strings.TrimSpace(file.Content) == "" && file.Chmod == "" && file.Chown == "" {
			continue
		}
		parsed, err := parseManagedFile(StackManagedConfigFile{Path: file.Path, Chmod: file.Chmod, Chown: file.Chown})
		if err != nil {
			return nil, err
		}
		if seen[parsed.Path] {
			continue
		}
		seen[parsed.Path] = true
		result = append(result, managedFileWrite{Path: parsed.Path, Content: file.Content, Chmod: parsed.Chmod, Chown: parsed.Chown})
	}
	return result, nil
}

func managedEnvFile(files []managedFileWrite) (string, bool) {
	for _, file := range files {
		if file.Path == ".env" {
			return strings.TrimRight(file.Content, "\r\n"), true
		}
	}
	return "", false
}

func managedFilesTrackEnv(files []managedFileWrite) bool {
	for _, file := range files {
		if file.Path == ".env" {
			return true
		}
	}
	return false
}

func managedConfigTracksEnv(cfg StackManagedConfig) bool {
	for _, file := range cfg.Files {
		if file.Path == ".env" {
			return true
		}
	}
	return false
}

func parseManagedConfigFiles(configFiles []StackManagedConfigFile) ([]StackManagedConfigFile, error) {
	var files []StackManagedConfigFile
	seen := make(map[string]bool)
	for _, configFile := range configFiles {
		file, err := parseManagedFile(configFile)
		if err != nil {
			return nil, err
		}
		if !seen[file.Path] {
			seen[file.Path] = true
			files = append(files, file)
		}
	}
	return files, nil
}

func parseManagedFile(file StackManagedConfigFile) (StackManagedConfigFile, error) {
	filePath, err := parseManagedFilePath(file.Path)
	if err != nil {
		return StackManagedConfigFile{}, err
	}
	parsed := StackManagedConfigFile{Path: filePath, Chmod: strings.TrimSpace(file.Chmod), Chown: strings.TrimSpace(file.Chown)}
	if parsed.Chmod != "" {
		if _, err := parseFileMode(parsed.Chmod); err != nil {
			return StackManagedConfigFile{}, err
		}
	}
	if parsed.Chown != "" {
		if _, _, err := parseFileOwner(parsed.Chown); err != nil {
			return StackManagedConfigFile{}, err
		}
	}
	return parsed, nil
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

func writeManagedFiles(stackDir string, folders []StackManagedConfigFolder, files []managedFileWrite) error {
	if err := validateManagedFileParents(stackDir, folders, files); err != nil {
		return err
	}
	for _, file := range files {
		filePath := filepath.Join(stackDir, filepath.FromSlash(file.Path))
		if err := ensurePathInside(stackDir, filePath); err != nil {
			return err
		}
		if err := ensureNoSymlinkInStackPath(stackDir, file.Path); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if file.Path == ".env" {
			mode = 0o600
		}
		if file.Chmod != "" {
			parsed, err := parseFileMode(file.Chmod)
			if err != nil {
				return err
			}
			mode = parsed
		}
		if err := os.WriteFile(filePath, []byte(file.Content), mode); err != nil {
			return err
		}
		if file.Path == ".env" || file.Chmod != "" {
			if err := os.Chmod(filePath, mode); err != nil {
				return err
			}
		}
		if file.Chown != "" {
			if runtime.GOOS == "windows" {
				return errWindowsChownUnsupported("managed file", file.Path)
			}
			if err := ensureResolvedPathInside(stackDir, filePath); err != nil {
				return err
			}
			uid, gid, err := parseFileOwner(file.Chown)
			if err != nil {
				return err
			}
			if err := os.Chown(filePath, uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateManagedFileParents(stackDir string, folders []StackManagedConfigFolder, files []managedFileWrite) error {
	allowedParents := map[string]bool{".": true}
	for _, folder := range folders {
		allowedParents[folder.Path] = true
	}
	for _, file := range files {
		parent := path.Dir(file.Path)
		if !allowedParents[parent] {
			parentPath := filepath.Join(stackDir, filepath.FromSlash(parent))
			info, err := os.Lstat(parentPath)
			if err != nil || !info.IsDir() {
				return fmt.Errorf("managed file parent folder does not exist: %s", parent)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("managed file parent folder cannot be a symlink: %s", parent)
			}
		}
		if parent != "." {
			if err := ensureNoSymlinkInStackPath(stackDir, parent); err != nil {
				return err
			}
		}
		if err := ensurePathInside(stackDir, filepath.Join(stackDir, filepath.FromSlash(file.Path))); err != nil {
			return err
		}
	}
	return nil
}

func parseManagedFolders(folders []StackManagedConfigFolder) ([]StackManagedConfigFolder, error) {
	result := make([]StackManagedConfigFolder, 0, len(folders))
	seen := make(map[string]bool)
	for _, folder := range folders {
		parsed, err := parseManagedFolder(folder)
		if err != nil {
			return nil, err
		}
		if parsed.Path == "" || seen[parsed.Path] {
			continue
		}
		seen[parsed.Path] = true
		result = append(result, parsed)
	}
	return result, nil
}

func parseManagedFolder(folder StackManagedConfigFolder) (StackManagedConfigFolder, error) {
	folderPath, err := cleanStackRelativePath(folder.Path, "bind folder")
	if err != nil {
		return StackManagedConfigFolder{}, err
	}
	parsed := StackManagedConfigFolder{Path: folderPath, Chmod: strings.TrimSpace(folder.Chmod), Chown: strings.TrimSpace(folder.Chown)}
	if parsed.Chmod != "" {
		if _, err := parseFolderMode(parsed.Chmod); err != nil {
			return StackManagedConfigFolder{}, err
		}
	}
	if parsed.Chown != "" {
		if _, _, err := parseFolderOwner(parsed.Chown); err != nil {
			return StackManagedConfigFolder{}, err
		}
	}
	return parsed, nil
}

func parseFolderMode(value string) (os.FileMode, error) {
	return parseManagedMode(value, "bind folder")
}

func parseFileMode(value string) (os.FileMode, error) {
	return parseManagedMode(value, "managed file")
}

func parseManagedMode(value, label string) (os.FileMode, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%s chmod is required", label)
	}
	if len(trimmed) > 4 {
		return 0, fmt.Errorf("%s chmod must be an octal mode: %s", label, value)
	}
	parsed, err := strconv.ParseUint(trimmed, 8, 32)
	if err != nil || parsed > 0o7777 {
		return 0, fmt.Errorf("%s chmod must be an octal mode: %s", label, value)
	}
	return os.FileMode(parsed), nil
}

func parseFolderOwner(value string) (int, int, error) {
	return parseManagedOwner(value, "bind folder")
}

func parseFileOwner(value string) (int, int, error) {
	return parseManagedOwner(value, "managed file")
}

func parseManagedOwner(value, label string) (int, int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return -1, -1, fmt.Errorf("%s chown is required", label)
	}
	uidText, gidText, hasColon := strings.Cut(trimmed, ":")
	if !hasColon || uidText == "" || gidText == "" {
		return -1, -1, fmt.Errorf("%s chown must use numeric uid:gid: %s", label, value)
	}
	uid, err := strconv.Atoi(uidText)
	if err != nil || uid < 0 {
		return -1, -1, fmt.Errorf("%s chown must use numeric uid:gid: %s", label, value)
	}
	gid, err := strconv.Atoi(gidText)
	if err != nil || gid < 0 {
		return -1, -1, fmt.Errorf("%s chown must use numeric uid:gid: %s", label, value)
	}
	return uid, gid, nil
}

func managedFolderPaths(folders []StackManagedConfigFolder) []string {
	paths := make([]string, 0, len(folders))
	for _, folder := range folders {
		paths = append(paths, folder.Path)
	}
	return paths
}
