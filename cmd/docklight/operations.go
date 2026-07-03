package main

import (
	"fmt"
	"strconv"
	"time"
)

const operationProgressInterval = time.Second

type operationProgress func(string)
type operationWork func(operationProgress) (string, error)

func (app *App) startStackOperation(stackName, action, startedOutput string, work operationWork) (int64, bool, error) {
	unlock, ok := app.tryLockStack(stackName)
	if !ok {
		return 0, false, nil
	}
	return app.startStackOperationLocked(stackName, action, startedOutput, unlock, work)
}

func (app *App) startStackOperationLocked(stackName, action, startedOutput string, unlock func(), work operationWork) (int64, bool, error) {
	app.operationMu.RLock()
	id, err := app.createActivity(stackName, action, "running", startedOutput)
	if err != nil {
		app.operationMu.RUnlock()
		unlock()
		return 0, false, err
	}
	go func() {
		defer app.operationMu.RUnlock()
		defer unlock()
		lastOutput := startedOutput
		progress := func(output string) {
			if output == lastOutput {
				return
			}
			lastOutput = output
			app.updateActivity(id, "running", output)
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				app.updateActivity(id, "failed", fmt.Sprintf("operation panicked: %v", recovered))
			}
		}()

		output, err := work(progress)
		status := "success"
		if err != nil {
			status = "failed"
		}
		app.updateActivity(id, status, output)
	}()
	return id, true, nil
}

func activityPending(status string) bool {
	return status == "running" || status == "started"
}

func operationStackPath(stackName string, activityID int64) string {
	return "/stacks/" + stackName + "?operation=" + strconv.FormatInt(activityID, 10)
}
