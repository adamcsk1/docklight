package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

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

func pathEscapesRoot(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}

func (app *App) writeStackFiles(name, composeYAML, envFile string, folders []string) error {
	stackDir, err := app.safeStackPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		return err
	}
	for _, folder := range folders {
		path := filepath.Join(stackDir, filepath.FromSlash(folder))
		if err := app.ensureStackPathSafe(path); err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	composePath := filepath.Join(stackDir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(composeYAML+"\n"), 0o644); err != nil {
		return err
	}
	return writeStackEnvFile(stackDir, envFile)
}

func (app *App) updateStackFiles(stack Stack, composeYAML, envFile string, folders []string, recreateFolders bool) error {
	if err := app.ensureStackPathSafe(stack.AbsolutePath); err != nil {
		return err
	}
	composePath := filepath.Join(stack.AbsolutePath, stack.ComposeFile)
	if err := ensurePathInside(stack.AbsolutePath, composePath); err != nil {
		return err
	}
	for _, folder := range folders {
		folderPath := filepath.Join(stack.AbsolutePath, filepath.FromSlash(folder))
		if err := app.ensureStackPathSafe(folderPath); err != nil {
			return err
		}
		if recreateFolders {
			if err := os.RemoveAll(folderPath); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(folderPath, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(composePath, []byte(composeYAML+"\n"), 0o644); err != nil {
		return err
	}
	return writeStackEnvFile(stack.AbsolutePath, envFile)
}

func writeStackEnvFile(stackDir, envFile string) error {
	path := filepath.Join(stackDir, ".env")
	if strings.TrimSpace(envFile) == "" {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return os.WriteFile(path, []byte(envFile+"\n"), 0o600)
}
