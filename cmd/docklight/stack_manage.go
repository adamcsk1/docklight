package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (app *App) createStack(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: "bad form"})
		return
	}

	form := newStackFormFromRequest(r)
	name, err := validateStackName(form.Name)
	if err != nil {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: err.Error(), StackForm: form})
		return
	}
	if form.ComposeYAML == "" {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: "compose yaml is required", StackForm: form})
		return
	}
	folders, err := parseBindFolders(form.BindFolders)
	if err != nil {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: err.Error(), StackForm: form})
		return
	}
	stackDir, err := app.safeStackPath(name)
	if err != nil {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: err.Error(), StackForm: form})
		return
	}
	if _, err := os.Stat(stackDir); err == nil {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: "stack folder already exists", StackForm: form})
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: err.Error(), StackForm: form})
		return
	}
	if err := os.Mkdir(stackDir, 0o755); err != nil {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: err.Error(), StackForm: form})
		return
	}
	if err := app.validateComposeDefinition(name, stackDir, form.ComposeYAML, form.EnvFile, form.ExternalBindMountsAllowed); err != nil {
		_ = os.RemoveAll(stackDir)
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: err.Error(), StackForm: form})
		return
	}

	if err := app.writeStackFiles(name, form.ComposeYAML, form.EnvFile, folders); err != nil {
		app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: err.Error(), StackForm: form})
		return
	}
	if err := app.rescanStacks(); err != nil {
		app.logActivity(name, "create stack", "failed", err.Error())
		app.renderStackCreateError(w, r, form, "stack files were created, but scan failed: "+err.Error())
		return
	}
	stack, err := app.loadStack(name, false)
	if err != nil {
		app.logActivity(name, "create stack", "failed", err.Error())
		app.renderStackCreateError(w, r, form, "stack files were created, but loading the stack failed: "+err.Error())
		return
	}
	if err := app.saveStackExternalBindMountsAllowed(stack.Name, form.ExternalBindMountsAllowed); err != nil {
		app.logActivity(name, "create stack", "failed", err.Error())
		app.renderStackCreateError(w, r, form, "stack files were created, but saving stack settings failed: "+err.Error())
		return
	}
	stack.ExternalBindMountsAllowed = form.ExternalBindMountsAllowed
	unlock := app.lockStack(stack.Name)
	defer unlock()
	output, err := app.runCompose(stack, "up", "-d")
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity(name, "create stack", status, output)
	if err != nil {
		app.setFlashCookie(w, r, "error", "stack files were created, but Docker start failed:\n"+output)
		http.Redirect(w, r, app.withBase("/stacks/"+name), http.StatusFound)
		return
	}
	app.redirectWithMessage(w, r, "/", "stack created and started: "+name)
}

func (app *App) editStackPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	stack, err := app.loadStack(name, false)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	composeYAML, err := os.ReadFile(filepath.Join(stack.AbsolutePath, stack.ComposeFile))
	if err != nil {
		app.redirectWithError(w, r, "/stacks/"+name, err.Error())
		return
	}
	envFile, err := os.ReadFile(filepath.Join(stack.AbsolutePath, ".env"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		app.redirectWithError(w, r, "/stacks/"+name, err.Error())
		return
	}
	app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, StackForm: StackForm{Name: stack.Name, ComposeYAML: strings.TrimRight(string(composeYAML), "\r\n"), EnvFile: strings.TrimRight(string(envFile), "\r\n"), ExternalBindMountsAllowed: stack.ExternalBindMountsAllowed}})
}

func (app *App) updateStack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	stack, err := app.loadStack(name, false)
	if err != nil {
		app.redirectWithError(w, r, "/", "stack not found: "+name)
		return
	}
	if stack.Protected || app.stackHasDocklightContainer(stack.Name) {
		app.redirectWithError(w, r, "/stacks/"+name, "Docklight cannot update its own stack")
		return
	}
	if err := r.ParseForm(); err != nil {
		app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, Error: "bad form"})
		return
	}
	form := updateStackFormFromRequest(r, stack.Name)
	if form.ComposeYAML == "" {
		app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, Error: "compose yaml is required", StackForm: form})
		return
	}
	if err := app.ensureStackPathSafe(stack.AbsolutePath); err != nil {
		app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, Error: err.Error(), StackForm: form})
		return
	}
	if err := ensurePathInside(stack.AbsolutePath, filepath.Join(stack.AbsolutePath, stack.ComposeFile)); err != nil {
		app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, Error: err.Error(), StackForm: form})
		return
	}
	if err := app.validateComposeDefinition(stack.Name, stack.AbsolutePath, form.ComposeYAML, form.EnvFile, form.ExternalBindMountsAllowed); err != nil {
		app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, Error: err.Error(), StackForm: form})
		return
	}
	folders, err := parseBindFolders(form.BindFolders)
	if err != nil {
		app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, Error: err.Error(), StackForm: form})
		return
	}
	if err := app.updateStackFiles(stack, form.ComposeYAML, form.EnvFile, folders, form.RecreateFolders); err != nil {
		app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, Error: err.Error(), StackForm: form})
		return
	}
	if err := app.saveStackExternalBindMountsAllowed(stack.Name, form.ExternalBindMountsAllowed); err != nil {
		app.render(w, r, "stack_edit.html", PageData{Title: "Edit " + stack.Name, Stack: stack, Error: err.Error(), StackForm: form})
		return
	}
	stack.ExternalBindMountsAllowed = form.ExternalBindMountsAllowed
	if r.FormValue("update_mode") == "save" {
		app.logActivity(name, "save stack", "success", "stack definition saved without recreate")
		app.redirectWithMessage(w, r, "/stacks/"+name, "stack changes saved")
		return
	}

	unlock := app.lockStack(stack.Name)
	defer unlock()
	outputDown, err := app.runCompose(stack, "down", "--rmi", "all", "--remove-orphans")
	if err != nil {
		app.logActivity(name, "update stack", "failed", outputDown)
		app.redirectWithError(w, r, "/stacks/"+name, outputDown)
		return
	}
	outputUp, err := app.runCompose(stack, "up", "-d", "--force-recreate")
	output := outputDown + "\n" + outputUp
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity(name, "update stack", status, output)
	if err != nil {
		app.redirectWithError(w, r, "/stacks/"+name, output)
		return
	}
	app.redirectWithMessage(w, r, "/stacks/"+name, "stack updated and recreated")
}

func (app *App) renderStackCreateError(w http.ResponseWriter, r *http.Request, form StackForm, message string) {
	app.render(w, r, "stack_new.html", PageData{Title: "New stack", Error: message, StackForm: form})
}

func (app *App) saveStackExternalBindMountsAllowed(name string, allowed bool) error {
	value := 0
	if allowed {
		value = 1
	}
	_, err := app.db.Exec(`UPDATE stacks SET external_bind_mounts_allowed = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`, value, name)
	return err
}

func (app *App) deleteStack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	stack, err := app.loadStack(name, false)
	if err != nil {
		app.redirectWithError(w, r, "/", "stack not found: "+name)
		return
	}
	if stack.Protected {
		app.redirectWithError(w, r, "/stacks/"+name, "Docklight cannot delete its own stack")
		return
	}
	if err := app.ensureStackPathSafe(stack.AbsolutePath); err != nil {
		app.redirectWithError(w, r, "/stacks/"+name, err.Error())
		return
	}

	unlock := app.lockStack(stack.Name)
	defer unlock()

	containers, err := app.dockerContainerResources(true)
	if err != nil {
		app.logActivity(name, "delete stack inspect", "failed", err.Error())
		app.redirectWithError(w, r, "/stacks/"+name, err.Error())
		return
	}
	if stackHasProtectedContainer(stack.Name, containers) {
		app.redirectWithError(w, r, "/stacks/"+name, "Docklight cannot delete its own stack")
		return
	}
	imageIDs := stackImageIDsFromContainers(stack.Name, containers)
	output, err := app.runCompose(stack, "down", "--remove-orphans", "--volumes")
	if err == nil {
		var imageOutput string
		imageOutput, err = app.removeUnusedImages(imageIDs)
		if imageOutput != "" {
			if output != "" {
				output += "\n"
			}
			output += imageOutput
		}
	}
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity(name, "delete stack", status, output)
	if err != nil {
		app.redirectWithError(w, r, "/stacks/"+name, output)
		return
	}
	if err := os.RemoveAll(stack.AbsolutePath); err != nil {
		app.logActivity(name, "delete stack files", "failed", err.Error())
		app.redirectWithError(w, r, "/stacks/"+name, err.Error())
		return
	}
	if _, err := app.db.Exec(`DELETE FROM stacks WHERE name = ?`, name); err != nil {
		app.logActivity(name, "delete stack database", "failed", err.Error())
		app.redirectWithError(w, r, "/", "stack files were deleted, but database cleanup failed: "+err.Error())
		return
	}
	app.setFlashCookie(w, r, "message", "stack deleted: "+name)
	http.Redirect(w, r, app.withBase("/"), http.StatusFound)
}
