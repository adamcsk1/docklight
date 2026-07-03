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
	if !app.operationMu.TryLock() {
		app.redirectWithError(w, r, "/settings", "database rebuild is blocked while a Docker operation is running")
		return
	}
	defer app.operationMu.Unlock()
	pending, err := app.hasPendingActivity()
	if err != nil {
		app.redirectWithError(w, r, "/settings", err.Error())
		return
	}
	if pending {
		app.redirectWithError(w, r, "/settings", "database rebuild is blocked while a Docker operation is running")
		return
	}
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
		app.startStackAction(w, r, stack, action, func(progress operationProgress) (string, error) {
			var output string
			stepProgress := prefixedProgress(&output, progress)
			outputDown, err := app.runComposeProgress(stack, stepProgress, "down", "--remove-orphans")
			output = outputDown
			if err != nil {
				return outputDown, err
			}
			stepProgress = prefixedProgress(&output, progress)
			outputUp, err := app.runComposeProgress(stack, stepProgress, "up", "-d", "--force-recreate")
			return outputDown + "\n" + outputUp, err
		})
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

	app.startStackAction(w, r, stack, action, func(progress operationProgress) (string, error) {
		return app.runComposeProgress(stack, progress, args...)
	})
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
	app.startStackAction(w, r, stack, action, func(progress operationProgress) (string, error) {
		return app.performStackUpgradeLocked(stack, recreate, progress)
	})
}

func (app *App) startStackAction(w http.ResponseWriter, r *http.Request, stack Stack, action string, work operationWork) {
	activityID, started, err := app.startStackOperation(stack.Name, action, action+" started", work)
	if err != nil {
		app.redirectWithError(w, r, "/stacks/"+stack.Name, action+" could not be scheduled: "+err.Error())
		return
	}
	if !started {
		app.redirectWithError(w, r, "/stacks/"+stack.Name, "another operation is already running for "+stack.Name)
		return
	}
	app.setFlashCookie(w, r, "message", action+" started")
	http.Redirect(w, r, app.withBase(operationStackPath(stack.Name, activityID)), http.StatusFound)
}

func (app *App) performStackUpgradeLocked(stack Stack, recreate bool, progress operationProgress) (string, error) {
	var output string
	stepProgress := prefixedProgress(&output, progress)
	outputPull, err := app.runComposeProgress(stack, stepProgress, "pull")
	if err != nil {
		return outputPull, err
	}
	output = outputPull
	outputs := []string{outputPull}
	upArgs := []string{"up", "-d"}
	if recreate {
		stepProgress = prefixedProgress(&output, progress)
		outputDown, err := app.runComposeProgress(stack, stepProgress, "down", "--remove-orphans")
		outputs = append(outputs, outputDown)
		output = joinOutputs(outputs)
		if err != nil {
			return output, err
		}
		upArgs = append(upArgs, "--force-recreate")
	}
	stepProgress = prefixedProgress(&output, progress)
	outputUp, err := app.runComposeProgress(stack, stepProgress, upArgs...)
	outputs = append(outputs, outputUp)
	output = joinOutputs(outputs)
	if err != nil {
		return output, err
	}
	return output, nil
}

func prefixedProgress(prefix *string, progress operationProgress) operationProgress {
	if progress == nil {
		return nil
	}
	return func(output string) {
		if *prefix == "" {
			progress(output)
			return
		}
		progress(*prefix + "\n" + output)
	}
}

func joinOutputs(outputs []string) string {
	output := ""
	for _, part := range outputs {
		if output != "" {
			output += "\n"
		}
		output += part
	}
	return output
}
