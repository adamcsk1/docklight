package main

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type Activity struct {
	ID        int64
	StackName string
	Action    string
	Status    string
	Output    string
	CreatedAt string
}

func (app *App) loadActivity() ([]Activity, error) {
	rows, err := app.db.Query(`SELECT id, stack_name, action, status, output, created_at FROM activity_log ORDER BY CASE WHEN lower(stack_name) = 'docklight' THEN 1 ELSE 0 END, id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var activities []Activity
	for rows.Next() {
		var activity Activity
		if err := rows.Scan(&activity.ID, &activity.StackName, &activity.Action, &activity.Status, &activity.Output, &activity.CreatedAt); err != nil {
			return nil, err
		}
		activities = append(activities, activity)
	}
	return activities, rows.Err()
}

func (app *App) logActivity(stackName, action, status, output string) {
	if len(output) > 12000 {
		output = output[:12000] + "\n... output truncated"
	}
	_, err := app.db.Exec(`INSERT INTO activity_log (stack_name, action, status, output) VALUES (?, ?, ?, ?)`, stackName, action, status, output)
	if err != nil {
		app.logger.Warn("failed to write activity", "error", err)
		app.verboseLog("activity insert failed", "error", err.Error())
		return
	}
	if err := app.purgeExpiredActivity(); err != nil {
		app.logger.Warn("failed to purge activity", "error", err)
		app.verboseLog("activity purge failed", "error", err.Error())
	}
}

func (app *App) purgeExpiredActivity() error {
	settings, err := app.loadSettings()
	if err != nil {
		return err
	}
	_, err = app.db.Exec(`DELETE FROM activity_log WHERE created_at < datetime('now', ?)`, fmt.Sprintf("-%d days", settings.ActivityRetentionDays))
	return err
}

func (app *App) downloadActivity(w http.ResponseWriter, r *http.Request) {
	rows, err := app.db.Query(`SELECT id, stack_name, action, status, output, created_at FROM activity_log ORDER BY id DESC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	filename := "docklight-activity-" + time.Now().Format("2006-01-02") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	writer := csv.NewWriter(w)
	if err := writer.Write([]string{"id", "created_at", "stack_name", "action", "status", "output"}); err != nil {
		return
	}
	for rows.Next() {
		var activity Activity
		if err := rows.Scan(&activity.ID, &activity.StackName, &activity.Action, &activity.Status, &activity.Output, &activity.CreatedAt); err != nil {
			return
		}
		if err := writer.Write([]string{strconv.FormatInt(activity.ID, 10), activity.CreatedAt, activity.StackName, activity.Action, activity.Status, activity.Output}); err != nil {
			return
		}
	}
	writer.Flush()
}
