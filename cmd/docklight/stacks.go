package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type stackStore interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

type Stack struct {
	Name                      string
	RelativePath              string
	ComposeFile               string
	AbsolutePath              string
	Status                    string
	CPUPercent                string
	MemoryUsage               string
	Protected                 bool
	ExternalBindMountsAllowed bool
	Containers                []Container
	ContainerCount            int
}

type Container struct {
	Name        string
	Service     string
	State       string
	Status      string
	Image       string
	CPUPercent  string
	MemoryUsage string
}

func (app *App) rescanStacks() error {
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := app.rescanStacksWith(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (app *App) rescanStacksWith(store stackStore) error {
	entries, err := os.ReadDir(app.cfg.StacksRoot)
	if err != nil {
		return err
	}
	found := make(map[string]bool)
	owners := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rel := entry.Name()
		composeFile := findComposeFile(filepath.Join(app.cfg.StacksRoot, rel))
		if composeFile == "" {
			continue
		}
		name := sanitizeStackName(rel)
		if name == "" {
			return fmt.Errorf("stack folder name %q does not produce a valid stack name", rel)
		}
		if owner, exists := owners[name]; exists && owner != rel {
			return fmt.Errorf("stack folder names %q and %q both map to stack name %q", owner, rel, name)
		}
		owners[name] = rel
		found[name] = true
		_, err := store.Exec(`
INSERT INTO stacks (name, relative_path, compose_file, ignored) VALUES (?, ?, ?, 0)
ON CONFLICT(name) DO UPDATE SET relative_path = excluded.relative_path, compose_file = excluded.compose_file, ignored = 0, external_bind_mounts_allowed = CASE WHEN stacks.ignored = 1 THEN 0 ELSE stacks.external_bind_mounts_allowed END, updated_at = CURRENT_TIMESTAMP
`, name, rel, composeFile)
		if err != nil {
			return err
		}
	}
	rows, err := store.Query(`SELECT name FROM stacks WHERE ignored = 0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !found[name] {
			if _, err := store.Exec(`UPDATE stacks SET ignored = 1, updated_at = CURRENT_TIMESTAMP WHERE name = ?`, name); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func findComposeFile(dir string) string {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name
		}
	}
	return ""
}

func sanitizeStackName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune('-')
	}
	return strings.Trim(b.String(), "-")
}

func (app *App) loadStacks(withLive bool) ([]Stack, error) {
	rows, err := app.db.Query(`SELECT name, relative_path, compose_file, external_bind_mounts_allowed FROM stacks WHERE ignored = 0 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stacks []Stack
	for rows.Next() {
		var stack Stack
		var externalBinds int
		if err := rows.Scan(&stack.Name, &stack.RelativePath, &stack.ComposeFile, &externalBinds); err != nil {
			return nil, err
		}
		stack.ExternalBindMountsAllowed = externalBinds == 1
		stack.Protected = app.protectedStackName(stack.Name)
		stack.AbsolutePath = filepath.Join(app.cfg.StacksRoot, stack.RelativePath)
		stack.Status = "unknown"
		stacks = append(stacks, stack)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if withLive {
		app.enrichStacksLive(stacks)
	}
	sort.Slice(stacks, func(i, j int) bool {
		if stacks[i].Protected != stacks[j].Protected {
			return !stacks[i].Protected
		}
		return stacks[i].Name < stacks[j].Name
	})
	return stacks, nil
}

func (app *App) loadStack(name string, withLive bool) (Stack, error) {
	var stack Stack
	var externalBinds int
	err := app.db.QueryRow(`SELECT name, relative_path, compose_file, external_bind_mounts_allowed FROM stacks WHERE name = ? AND ignored = 0`, name).Scan(&stack.Name, &stack.RelativePath, &stack.ComposeFile, &externalBinds)
	if err != nil {
		return stack, err
	}
	stack.ExternalBindMountsAllowed = externalBinds == 1
	stack.Protected = app.protectedStackName(stack.Name)
	stack.AbsolutePath = filepath.Join(app.cfg.StacksRoot, stack.RelativePath)
	stack.Status = "unknown"
	if withLive {
		app.enrichLive(&stack)
	}
	return stack, nil
}

func (app *App) protectedStackName(name string) bool {
	return strings.EqualFold(name, "docklight")
}

func (app *App) stackHasDocklightContainer(name string) bool {
	containers, err := app.dockerContainerResources(true)
	if err != nil {
		return false
	}
	return stackHasProtectedContainer(name, containers)
}

func stackHasProtectedContainer(name string, containers []DockerContainer) bool {
	for _, container := range containers {
		if container.Protected && (strings.EqualFold(container.Stack, name) || strings.EqualFold(container.Stack, "external:"+name)) {
			return true
		}
	}
	return false
}

func (app *App) enrichLive(stack *Stack) {
	containers, err := app.composeContainers(*stack)
	if err != nil {
		stack.Status = "unknown"
		return
	}
	stack.Containers = containers
	stack.ContainerCount = len(containers)
	for _, container := range containers {
		if strings.EqualFold(container.Name, "docklight") || strings.EqualFold(container.Service, "docklight") {
			stack.Protected = true
			break
		}
	}
	if len(containers) == 0 {
		stack.Status = "stopped"
	} else {
		stack.Status = aggregateStatus(containers)
	}
}

func (app *App) enrichStacksLive(stacks []Stack) {
	inspected, err := app.dockerContainerInspects(true)
	if err != nil {
		return
	}
	containersByStack := make(map[string][]Container)
	protectedByStack := make(map[string]bool)
	for _, item := range inspected {
		labels := item.Config.Labels
		project := strings.TrimSpace(labels[composeProjectLabel])
		if project == "" {
			continue
		}
		container := Container{
			Name:    strings.TrimPrefix(item.Name, "/"),
			Service: strings.TrimSpace(labels[composeServiceLabel]),
			State:   item.State.Status,
			Status:  item.State.Status,
			Image:   item.Config.Image,
		}
		containersByStack[project] = append(containersByStack[project], container)
		if app.isDocklightContainerInspect(item) {
			protectedByStack[project] = true
		}
	}
	for i := range stacks {
		containers := sortComposeContainers(containersByStack[stacks[i].Name])
		stacks[i].Containers = containers
		stacks[i].ContainerCount = len(containers)
		if protectedByStack[stacks[i].Name] {
			stacks[i].Protected = true
		}
		if len(containers) == 0 {
			stacks[i].Status = "stopped"
		} else {
			stacks[i].Status = aggregateStatus(containers)
		}
	}
}

func (app *App) enrichStackStats(stack *Stack) {
	stats, err := app.dockerContainerStats(runningComposeContainerNames(stack.Containers))
	if err != nil {
		return
	}
	stack.Containers = enrichComposeContainerStats(stack.Containers, stats)
	stack.CPUPercent, stack.MemoryUsage = summarizeComposeContainerStats(stack.Containers)
}

func (app *App) enrichStacksStats(stacks []Stack) {
	var names []string
	for _, stack := range stacks {
		names = append(names, runningComposeContainerNames(stack.Containers)...)
	}
	stats, err := app.dockerContainerStats(names)
	if err != nil {
		return
	}
	for i := range stacks {
		stacks[i].Containers = enrichComposeContainerStats(stacks[i].Containers, stats)
		stacks[i].CPUPercent, stacks[i].MemoryUsage = summarizeComposeContainerStats(stacks[i].Containers)
	}
}

func aggregateStatus(containers []Container) string {
	running := 0
	for _, container := range containers {
		if strings.EqualFold(container.State, "running") {
			running++
		}
	}
	if running == len(containers) {
		return "running"
	}
	if running == 0 {
		return "stopped"
	}
	return "partial"
}

func (app *App) stackNameSet() (map[string]bool, error) {
	rows, err := app.db.Query(`SELECT name FROM stacks WHERE ignored = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = true
	}
	return names, rows.Err()
}
