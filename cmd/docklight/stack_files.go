package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var chownPath = os.Chown

func (app *App) safeStackPath(name string) (string, error) {
	if _, err := validateStackName(name); err != nil {
		return "", err
	}
	return filepath.Join(app.cfg.StacksRoot, name), nil
}

func (app *App) ensureStackPathSafe(path string) error {
	root, err := filepath.Abs(app.cfg.StacksRoot)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if target == root {
		return errors.New("refusing to remove stacks root")
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if pathEscapesRoot(rel) {
		return errors.New("refusing to operate outside stacks root")
	}
	info, err := os.Lstat(target)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to operate on symlinked stack path")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func ensurePathInside(rootPath, targetPath string) error {
	root, err := filepath.Abs(rootPath)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == "." || pathEscapesRoot(rel) {
		return errors.New("refusing to operate outside stack folder")
	}
	return nil
}

func ensureResolvedPathInside(rootPath, targetPath string) error {
	root, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return err
	}
	target, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return err
	}
	return ensurePathInside(root, target)
}

func pathEscapesRoot(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}

func (app *App) writeStackFiles(name, composeYAML string, folders []StackManagedConfigFolder, managedFiles []managedFileWrite, settings StackManagedSettings) error {
	stackDir, err := app.safeStackPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		return err
	}
	if err := app.chownStackPath(stackDir); err != nil {
		return err
	}
	for _, folder := range folders {
		path := filepath.Join(stackDir, filepath.FromSlash(folder.Path))
		if err := app.ensureStackPathSafe(path); err != nil {
			return err
		}
		if err := app.createManagedFolder(stackDir, path, folder); err != nil {
			return err
		}
	}
	if err := validateManagedFileParents(stackDir, folders, managedFiles); err != nil {
		return err
	}
	composePath := filepath.Join(stackDir, "docker-compose.yml")
	if err := ensureNoSymlinkInStackPath(stackDir, "docker-compose.yml"); err != nil {
		return err
	}
	if err := os.WriteFile(composePath, []byte(composeYAML+"\n"), 0o644); err != nil {
		return err
	}
	if err := app.chownStackPath(composePath); err != nil {
		return err
	}
	if err := app.writeManagedFiles(stackDir, folders, managedFiles); err != nil {
		return err
	}
	return app.writeManagedStackConfig(stackDir, folders, managedFiles, settings)
}

func (app *App) updateStackFiles(stack Stack, composeYAML string, folders []StackManagedConfigFolder, managedFiles []managedFileWrite, settings StackManagedSettings) error {
	if err := app.ensureStackPathSafe(stack.AbsolutePath); err != nil {
		return err
	}
	previousConfig, err := app.loadManagedStackConfig(stack.AbsolutePath)
	if err != nil {
		return err
	}
	composePath := filepath.Join(stack.AbsolutePath, stack.ComposeFile)
	if err := ensurePathInside(stack.AbsolutePath, composePath); err != nil {
		return err
	}
	if err := ensureNoSymlinkInStackPath(stack.AbsolutePath, stack.ComposeFile); err != nil {
		return err
	}
	if err := ensureResolvedPathInside(stack.AbsolutePath, composePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, folder := range folders {
		folderPath := filepath.Join(stack.AbsolutePath, filepath.FromSlash(folder.Path))
		if err := app.ensureStackPathSafe(folderPath); err != nil {
			return err
		}
		if err := app.createManagedFolder(stack.AbsolutePath, folderPath, folder); err != nil {
			return err
		}
	}
	if err := validateManagedFileParents(stack.AbsolutePath, folders, managedFiles); err != nil {
		return err
	}
	if err := os.WriteFile(composePath, []byte(composeYAML+"\n"), 0o644); err != nil {
		return err
	}
	if err := app.chownStackPath(composePath); err != nil {
		return err
	}
	if err := app.writeManagedFiles(stack.AbsolutePath, folders, managedFiles); err != nil {
		return err
	}
	if managedConfigTracksEnv(previousConfig) && !managedFilesTrackEnv(managedFiles) {
		err := os.Remove(filepath.Join(stack.AbsolutePath, ".env"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return app.writeManagedStackConfig(stack.AbsolutePath, folders, managedFiles, settings)
}

func (app *App) createManagedFolder(stackDir, folderPath string, folder StackManagedConfigFolder) error {
	if err := ensureNoSymlinkInStackPath(stackDir, folder.Path); err != nil {
		return err
	}
	mode := os.FileMode(0o755)
	if folder.Chmod != "" {
		parsed, err := parseFolderMode(folder.Chmod)
		if err != nil {
			return err
		}
		mode = parsed
	}
	if err := os.MkdirAll(filepath.Dir(folderPath), 0o755); err != nil {
		return err
	}
	if err := app.chownStackPathParents(stackDir, folderPath); err != nil {
		return err
	}
	if err := os.Mkdir(folderPath, mode); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		info, statErr := os.Lstat(folderPath)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed folder cannot use symlink path: %s", folder.Path)
		}
		if !info.IsDir() {
			return err
		}
	}
	if err := ensureNoSymlinkInStackPath(stackDir, folder.Path); err != nil {
		return err
	}
	if err := ensureResolvedPathInside(stackDir, folderPath); err != nil {
		return err
	}
	if folder.Chmod != "" {
		if err := os.Chmod(folderPath, mode); err != nil {
			return err
		}
	}
	if folder.Chown != "" {
		if runtime.GOOS == "windows" {
			return errWindowsChownUnsupported("bind folder", folder.Path)
		}
		uid, gid, err := parseFolderOwner(folder.Chown)
		if err != nil {
			return err
		}
		if err := chownPath(folderPath, uid, gid); err != nil {
			return err
		}
	} else if err := app.chownStackPath(folderPath); err != nil {
		return err
	}
	return nil
}

func (app *App) chownStackPath(targetPath string) error {
	if app.cfg.StackFileOwner == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		return errWindowsChownUnsupported("STACKS_FILE_OWNER", targetPath)
	}
	return chownPath(targetPath, app.cfg.StackFileOwner.UID, app.cfg.StackFileOwner.GID)
}

func (app *App) chownStackPathParents(stackDir, targetPath string) error {
	if app.cfg.StackFileOwner == nil {
		return nil
	}
	parentPath := filepath.Dir(targetPath)
	rel, err := filepath.Rel(stackDir, parentPath)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if pathEscapesRoot(rel) {
		return errors.New("refusing to operate outside stack folder")
	}
	current := stackDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := app.chownStackPath(current); err != nil {
			return err
		}
	}
	return nil
}

func ensureNoSymlinkInStackPath(stackDir, relativePath string) error {
	info, err := os.Lstat(stackDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("stack folder cannot be a symlink: %s", stackDir)
	}
	current := stackDir
	for _, part := range strings.Split(filepath.FromSlash(relativePath), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed folder cannot use symlink path: %s", relativePath)
		}
	}
	return nil
}
