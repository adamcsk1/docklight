package main

import "sync"

func (app *App) lockStack(name string) func() {
	app.locksMu.Lock()
	if app.locks == nil {
		app.locks = make(map[string]*sync.Mutex)
	}
	lock := app.locks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		app.locks[name] = lock
	}
	app.locksMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (app *App) tryLockStack(name string) (func(), bool) {
	app.locksMu.Lock()
	if app.locks == nil {
		app.locks = make(map[string]*sync.Mutex)
	}
	lock := app.locks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		app.locks[name] = lock
	}
	app.locksMu.Unlock()
	if !lock.TryLock() {
		return nil, false
	}
	return lock.Unlock, true
}
