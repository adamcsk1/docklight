package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"docklight/internal/imageupdates"
	"docklight/internal/management"
)

func (app *App) initImageUpdates() error {
	if !app.cfg.Updates.Enabled {
		return nil
	}
	if app.masterHost() == nil {
		return errors.New("image update checks require one master host")
	}
	m := &imageupdates.Monitor{Config: app.cfg.Updates, Resolve: imageupdates.Registry{}.Resolve, ResolveManifest: imageupdates.Registry{}.ResolveManifest, Allowed: app.imageUpdatesAllowed}
	for _, host := range app.cfg.Hosts {
		host := host
		m.Sources = append(m.Sources, imageupdates.Source{ID: host.ID, Name: host.Name, Address: host.URL, Fetch: func(ctx context.Context) (management.ImageSnapshot, error) {
			var snapshot management.ImageSnapshot
			r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://coordinator.local", nil)
			err := host.request(r, http.MethodGet, "/v1/image-updates", nil, &snapshot)
			return snapshot, err
		}})
	}
	m.Save = func(state imageupdates.State) error {
		b, err := json.Marshal(state)
		if err != nil {
			return err
		}
		_, err = app.db.Exec(`INSERT INTO image_updates(id,value) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET value=excluded.value`, string(b))
		return err
	}
	if app.cfg.Updates.SMTP.Host != "" {
		m.Email = app.cfg.Updates.SMTP.Send
	}
	if app.cfg.Updates.MQTT.Broker != "" {
		m.MQTT = app.cfg.Updates.MQTT.Send
	}
	var state imageupdates.State
	var text string
	err := app.db.QueryRow(`SELECT value FROM image_updates WHERE id=1`).Scan(&text)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return errors.New("cannot read image update state")
	}
	if err == nil && json.Unmarshal([]byte(text), &state) != nil {
		return errors.New("invalid persisted image update state; restore database backup")
	}
	if err == nil && state.Version != 1 {
		return errors.New("unsupported persisted image update state version")
	}
	if err = m.Restore(state); err != nil {
		return err
	}
	app.imageMonitor = m
	app.imageCheckWake = make(chan struct{}, 1)
	return nil
}

func (app *App) imageUpdateLoop(ctx context.Context) {
	m := app.imageMonitor
	if m == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var checked time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UTC()
		manual := app.runRequestedImageCheck(ctx)
		if !manual && app.imageUpdatesAllowed() && (checked.IsZero() || now.Sub(checked) >= m.Config.Interval) {
			checkCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			if err := m.Check(checkCtx, now); err != nil {
				log.Print("image update check incomplete or state persistence failed")
			}
			cancel()
			checked = now
		}
		if app.imageUpdatesAllowed() {
			deliveryCtx, stop := context.WithTimeout(ctx, time.Minute)
			if err := m.Deliver(deliveryCtx, time.Now().UTC()); err != nil {
				log.Print("image update notification or state persistence failed; retry scheduled")
			}
			stop()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-app.imageCheckWake:
		}
	}
}

func (app *App) imageUpdatesAllowed() bool {
	app.verificationMu.RLock()
	verified, conflict, checked := app.verification.OwnershipVerified, app.verification.Conflict, app.verifiedAt
	app.verificationMu.RUnlock()
	if !verified || conflict || checked.IsZero() || time.Since(checked) > 2*time.Minute || time.Now().Before(checked) {
		return false
	}
	var value string
	err := app.db.QueryRow(`SELECT value FROM maintenance WHERE key='conflict'`).Scan(&value)
	return errors.Is(err, sql.ErrNoRows)
}

func (app *App) populateImageUpdates(h *Host, d *PageData) {
	d.ImageUpdates = map[string]imageupdates.Result{}
	d.ResourceUpdates = map[string][]imageupdates.Result{}
	if app.imageMonitor == nil {
		d.UpdatesSummary = "Image update checks disabled"
		return
	}
	state := app.imageMonitor.Host(h.ID, h.URL)
	d.UpdatesSummary = "Image updates checked " + state.Checked.UTC().Format(time.RFC3339)
	if state.Error != "" {
		d.UpdatesSummary = state.Error
	} else if state.SnapshotVersion != management.ImageSnapshotVersion {
		d.UpdatesSummary = "Image update metadata pending; upgrade agent if checks remain unavailable"
	}
	stale := state.Checked.IsZero() || time.Since(state.Checked) > app.cfg.Updates.Interval+5*time.Minute || time.Now().Before(state.Checked)
	if stale && state.Error == "" && state.SnapshotVersion == management.ImageSnapshotVersion {
		d.UpdatesSummary = "Image update results stale; last check " + state.Checked.UTC().Format(time.RFC3339)
	}
	results := make(map[string]imageupdates.Result, len(state.Results))
	if state.SnapshotVersion == management.ImageSnapshotVersion {
		for _, result := range state.Results {
			results[result.ID] = result
		}
	}
	for _, container := range d.Inventory.Containers {
		result := imageupdates.Result{ImageContainer: management.ImageContainer{ID: container.ID, Name: container.Name, Image: container.Image, ImageID: container.ImageID}, Status: "unknown", Detail: "Update check pending for deployed image"}
		if cached, ok := results[container.ID]; ok {
			if cached.ImageID == container.ImageID && cached.Image == container.Image {
				result = cached
				if stale {
					result.Status = "stale"
					result.Detail = "Last result: " + cached.Status + "; awaiting fresh check"
				}
			}
		}
		d.ImageUpdates[container.ID] = result
		if container.ImageID != "" {
			duplicate := false
			for i, existing := range d.ResourceUpdates[container.ImageID] {
				if existing.Image == result.Image {
					duplicate = true
					if result.Status == "available" {
						d.ResourceUpdates[container.ImageID][i] = result
					}
				}
			}
			if !duplicate {
				d.ResourceUpdates[container.ImageID] = append(d.ResourceUpdates[container.ImageID], result)
			}
		}
	}
}

func (app *App) groupImageUpdates(d *PageData) {
	for i := range d.ContainerGroups {
		services := map[string]bool{}
		for _, c := range d.ContainerGroups[i].Containers {
			r := d.ImageUpdates[c.ID]
			if r.Status == "available" {
				key := c.Service
				if d.ContainerGroups[i].Project == "" || key == "" {
					key = c.ID
				}
				services[key] = true
			} else if r.Status == "unknown" || r.Status == "stale" {
				d.ContainerGroups[i].UnknownUpdates++
			}
		}
		d.ContainerGroups[i].Updates = len(services)
	}
}
