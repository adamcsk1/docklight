package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type composePSContainer struct {
	Name    string
	Service string
	State   string
	Status  string
	Image   string
}

func (app *App) composeContainers(stack Stack) ([]Container, error) {
	output, err := app.runCompose(stack, "ps", "-a", "--format", "json")
	if err != nil {
		output, err = app.runCompose(stack, "ps", "--format", "{{.Name}}|{{.Service}}|{{.State}}|{{.Status}}|{{.Image}}")
		if err != nil {
			return nil, err
		}
		return sortComposeContainers(parseComposePSLines(output)), nil
	}
	containers, err := parseComposePSJSON(output)
	if err != nil {
		return nil, err
	}
	return sortComposeContainers(containers), nil
}

func (app *App) runCompose(stack Stack, args ...string) (string, error) {
	composePath := filepath.Join(stack.AbsolutePath, stack.ComposeFile)
	if err := app.ensureStackPathSafe(stack.AbsolutePath); err != nil {
		return "", err
	}
	if err := ensurePathInside(stack.AbsolutePath, composePath); err != nil {
		return "", fmt.Errorf("unsafe compose file path: %w", err)
	}
	composeFile, cleanup, err := app.composeFileForCommand(stack, composePath)
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmdArgs := append([]string{"compose", "--project-name", stack.Name, "--project-directory", stack.AbsolutePath, "-f", composeFile}, args...)
	return app.runCommand(stack.AbsolutePath, "docker", cmdArgs...)
}

func (app *App) composeFileForCommand(stack Stack, composePath string) (string, func(), error) {
	hostRoot := app.cfg.StacksHostRoot
	if hostRoot == "" || hostRoot == app.cfg.StacksRoot {
		return composePath, func() {}, nil
	}
	relativePath, err := app.stackRelativePath(stack)
	if err != nil {
		return "", func() {}, err
	}
	hostStackPath := filepath.Join(hostRoot, relativePath)
	if err := ensurePathInside(hostRoot, hostStackPath); err != nil {
		return "", func() {}, fmt.Errorf("unsafe host stack path: %w", err)
	}

	content, err := os.ReadFile(composePath)
	if err != nil {
		return "", func() {}, err
	}
	rewriteContent := content
	if resolved, err := app.composeConfigForRewrite(stack, composePath); err == nil && strings.TrimSpace(resolved) != "" {
		rewriteContent = []byte(resolved)
	}
	rewritten, changed, err := rewriteComposeRelativeBinds(rewriteContent, filepath.ToSlash(hostStackPath), filepath.ToSlash(stack.AbsolutePath))
	if err != nil {
		return "", func() {}, err
	}
	if !changed && string(rewriteContent) != string(content) {
		rewritten, changed, err = rewriteComposeRelativeBinds(content, filepath.ToSlash(hostStackPath), filepath.ToSlash(stack.AbsolutePath))
		if err != nil {
			return "", func() {}, err
		}
	}
	if !changed {
		return composePath, func() {}, nil
	}
	tmp, err := os.CreateTemp(stack.AbsolutePath, ".docklight-compose-*.yml")
	if err != nil {
		return "", func() {}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(rewritten); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", func() {}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", func() {}, err
	}
	return tmpName, func() { _ = os.Remove(tmpName) }, nil
}

func (app *App) composeConfigForRewrite(stack Stack, composePath string) (string, error) {
	args := []string{"compose", "--project-name", stack.Name, "--project-directory", stack.AbsolutePath, "-f", composePath, "config"}
	return app.runCommand(stack.AbsolutePath, "docker", args...)
}

func (app *App) stackRelativePath(stack Stack) (string, error) {
	relativePath := stack.RelativePath
	if relativePath == "" {
		var err error
		relativePath, err = filepath.Rel(app.cfg.StacksRoot, stack.AbsolutePath)
		if err != nil {
			return "", err
		}
	}
	if relativePath == "." || pathEscapesRoot(relativePath) {
		return "", fmt.Errorf("unsafe stack relative path: %q", relativePath)
	}
	return relativePath, nil
}
