package main

import (
	"net/http"
	"strconv"
)

func (app *App) rescan(w http.ResponseWriter, r *http.Request) {
	if err := app.rescanStacks(); err != nil {
		app.logActivity("system", "rescan", "failed", err.Error())
		app.redirectWithError(w, r, "/", err.Error())
		return
	}
	app.logActivity("system", "rescan", "success", "Stack folder rescan completed")
	app.redirectWithMessage(w, r, "/", "stack folder scan completed")
}

func (app *App) rebuildDatabase(w http.ResponseWriter, r *http.Request) {
	if err := app.resetDatabase(); err != nil {
		app.redirectWithError(w, r, "/settings", err.Error())
		return
	}
	app.logActivity("system", "database rebuild", "success", "Database rebuilt from filesystem")
	app.redirectWithMessage(w, r, "/settings", "database rebuilt")
}

func (app *App) resetDatabase() error {
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
DROP TABLE IF EXISTS stacks;
DROP TABLE IF EXISTS activity_log;
DROP TABLE IF EXISTS app_settings;
DROP TABLE IF EXISTS sessions;
PRAGMA user_version = 0;
`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := app.initDB(); err != nil {
		return err
	}
	return app.rescanStacks()
}

func (app *App) updateSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		app.redirectWithError(w, r, "/settings", err.Error())
		return
	}
	retentionDays, err := strconv.Atoi(r.FormValue("activity_retention_days"))
	if err != nil || !validRetentionDays(retentionDays) {
		app.redirectWithError(w, r, "/settings", "invalid activity retention")
		return
	}
	verboseRetentionDays, err := strconv.Atoi(r.FormValue("verbose_log_retention_days"))
	if err != nil || !validRetentionDays(verboseRetentionDays) {
		app.redirectWithError(w, r, "/settings", "invalid verbose log retention")
		return
	}
	settings := AppSettings{
		ActivityRetentionDays:   retentionDays,
		VerboseLogRetentionDays: verboseRetentionDays,
		VerboseLoggingEnabled:   r.FormValue("verbose_logging_enabled") == "on",
	}
	if err := app.saveSettings(settings); err != nil {
		app.redirectWithError(w, r, "/settings", err.Error())
		return
	}
	if err := app.purgeExpiredActivity(); err != nil {
		app.redirectWithError(w, r, "/settings", err.Error())
		return
	}
	if err := app.purgeExpiredVerboseLogs(); err != nil {
		app.redirectWithError(w, r, "/settings", err.Error())
		return
	}
	app.verboseLog("settings updated", "activity_retention_days", strconv.Itoa(settings.ActivityRetentionDays), "verbose_log_retention_days", strconv.Itoa(settings.VerboseLogRetentionDays), "verbose_logging_enabled", strconv.FormatBool(settings.VerboseLoggingEnabled))
	app.redirectWithMessage(w, r, "/settings", "settings saved")
}

func (app *App) stackAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	action := r.PathValue("action")
	stack, err := app.loadStack(name, false)
	if err != nil {
		app.redirectWithError(w, r, "/", "stack not found: "+name)
		return
	}
	if (stack.Protected || app.stackHasDocklightContainer(stack.Name)) && protectedStackAction(action) {
		app.redirectWithError(w, r, "/stacks/"+name, "Docklight cannot run this action against its own stack")
		return
	}

	var args []string
	switch action {
	case "start":
		args = []string{"up", "-d"}
	case "stop":
		args = []string{"stop"}
	case "restart":
		args = []string{"restart"}
	case "rebuild":
		args = []string{"up", "-d", "--build"}
	case "recreate":
		unlock := app.lockStack(stack.Name)
		defer unlock()

		outputDown, err := app.runCompose(stack, "down", "--remove-orphans")
		status := "success"
		if err != nil {
			status = "failed"
			app.logActivity(name, action, status, outputDown)
			app.redirectWithError(w, r, "/stacks/"+name, outputDown)
			return
		}
		outputUp, err := app.runCompose(stack, "up", "-d", "--force-recreate")
		output := outputDown + "\n" + outputUp
		if err != nil {
			status = "failed"
		}
		app.logActivity(name, action, status, output)
		if err != nil {
			app.redirectWithError(w, r, "/stacks/"+name, output)
			return
		}
		app.redirectWithMessage(w, r, "/stacks/"+name, "stack recreated")
		return
	case "upgrade":
		app.runStackUpgrade(w, r, stack, action, false)
		return
	case "upgrade-recreate":
		app.runStackUpgrade(w, r, stack, action, true)
		return
	case "down":
		args = []string{"down"}
	default:
		app.redirectWithError(w, r, "/stacks/"+name, "unknown stack action: "+action)
		return
	}

	unlock := app.lockStack(stack.Name)
	defer unlock()

	output, err := app.runCompose(stack, args...)
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity(name, action, status, output)
	if err != nil {
		app.redirectWithError(w, r, "/stacks/"+name, output)
		return
	}
	app.redirectWithMessage(w, r, "/stacks/"+name, action+" completed")
}

func protectedStackAction(action string) bool {
	switch action {
	case "start", "stop", "restart", "rebuild", "recreate", "upgrade", "upgrade-recreate", "down":
		return true
	default:
		return false
	}
}

func (app *App) runStackUpgrade(w http.ResponseWriter, r *http.Request, stack Stack, action string, recreate bool) {
	unlock := app.lockStack(stack.Name)
	defer unlock()
	output, err := app.performStackUpgradeLocked(stack, recreate)
	status := "success"
	if err != nil {
		status = "failed"
	}
	app.logActivity(stack.Name, action, status, output)
	if err != nil {
		app.redirectWithError(w, r, "/stacks/"+stack.Name, output)
		return
	}
	app.redirectWithMessage(w, r, "/stacks/"+stack.Name, action+" completed")
}

func (app *App) performStackUpgradeLocked(stack Stack, recreate bool) (string, error) {
	outputPull, err := app.runCompose(stack, "pull")
	if err != nil {
		return outputPull, err
	}
	outputs := []string{outputPull}
	upArgs := []string{"up", "-d"}
	if recreate {
		outputDown, err := app.runCompose(stack, "down", "--remove-orphans")
		outputs = append(outputs, outputDown)
		if err != nil {
			return outputPull + "\n" + outputDown, err
		}
		upArgs = append(upArgs, "--force-recreate")
	}
	outputUp, err := app.runCompose(stack, upArgs...)
	outputs = append(outputs, outputUp)
	output := ""
	for _, part := range outputs {
		if output != "" {
			output += "\n"
		}
		output += part
	}
	if err != nil {
		return output, err
	}
	return output, nil
}
