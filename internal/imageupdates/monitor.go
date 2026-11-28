package imageupdates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"docklight/internal/management"
)

type Source struct {
	ID, Name, Address string
	Fetch             func(context.Context) (management.ImageSnapshot, error)
}

type Result struct {
	management.ImageContainer
	Status, Available, Detail string
	Checked                   time.Time
}

type HostState struct {
	SnapshotVersion                  int
	Address, NodeID, Hostname, Error string
	Checked                          time.Time
	Results                          []Result
}

type HostIdentity struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Hostname string `json:"hostname"`
}

type Target struct {
	Container string `json:"container"`
	Stack     string `json:"stack"`
	Service   string `json:"service"`
}

type Event struct {
	ID        string       `json:"id"`
	Type      string       `json:"type"`
	Host      HostIdentity `json:"host"`
	Image     string       `json:"image"`
	Platform  string       `json:"platform"`
	Deployed  string       `json:"deployed_sha"`
	Available string       `json:"available_sha"`
	Detected  time.Time    `json:"detected_at"`
	Targets   []Target     `json:"targets"`
}

type Delivery struct {
	HostID          string
	Image, Platform string
	Email, MQTT     bool
}

type State struct {
	Version int
	Hosts   map[string]HostState
	Sent    map[string]Delivery
}

type Monitor struct {
	Config          Config
	Sources         []Source
	Resolve         func(context.Context, string, string, string, string) (string, error)
	ResolveManifest func(context.Context, string, string, string, string) (string, error)
	Save            func(State) error
	Email           func(context.Context, []Event) error
	MQTT            func(context.Context, Event) error
	Allowed         func() bool
	mu              sync.RWMutex
	work            sync.Mutex
	state           State
	deliveryAfter   string
}

func (m *Monitor) Restore(state State) error {
	if state.Version != 0 && state.Version != 1 {
		return errors.New("unsupported image update state version")
	}
	if state.Version == 1 && (state.Hosts == nil || state.Sent == nil) {
		return errors.New("invalid persisted image update state")
	}
	if state.Hosts == nil {
		state.Hosts = map[string]HostState{}
	}
	if state.Sent == nil {
		state.Sent = map[string]Delivery{}
	}
	state.Version = 1
	state = cloneState(state)
	// Older delivery records can recover their image identity from the cached result.
	for id, delivery := range state.Sent {
		if delivery.Image != "" {
			continue
		}
		host := state.Hosts[delivery.HostID]
		for _, result := range host.Results {
			if eventID(host.NodeID, result) == id {
				delivery.Image, delivery.Platform = result.Image, resultPlatform(result)
				state.Sent[id] = delivery
				break
			}
		}
	}
	m.work.Lock()
	defer m.work.Unlock()
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
	return nil
}

func (m *Monitor) Host(id, address string) HostState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h := m.state.Hosts[id]
	if h.Address != address {
		return HostState{Error: "Update check pending"}
	}
	h.Results = append([]Result(nil), h.Results...)
	return h
}

func (m *Monitor) copyState() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneState(m.state)
}

func cloneState(state State) State {
	state.Hosts = maps.Clone(state.Hosts)
	state.Sent = maps.Clone(state.Sent)
	for id, host := range state.Hosts {
		host.Results = slices.Clone(host.Results)
		state.Hosts[id] = host
	}
	return state
}

func (m *Monitor) publish(state State) error {
	err := m.Save(state)
	m.mu.Lock()
	m.state = cloneState(state)
	m.mu.Unlock()
	return err
}

func (m *Monitor) Check(ctx context.Context, now time.Time) error {
	return m.CheckSelected(ctx, now, nil)
}

// CheckSelected refreshes only selected sources; nil retains the scheduled full-fleet scan.
func (m *Monitor) CheckSelected(ctx context.Context, now time.Time, selected []string) error {
	m.work.Lock()
	defer m.work.Unlock()
	state := m.copyState()
	type resolved struct {
		digest string
		err    error
	}
	cache := map[string]resolved{}
	throttled := map[string]error{}
	var interrupted error
	for _, source := range m.Sources {
		if selected != nil && !slices.Contains(selected, source.ID) {
			continue
		}
		if interrupted == nil {
			interrupted = m.workAllowed(ctx)
		}
		if interrupted != nil {
			state.Hosts[source.ID] = HostState{Address: source.Address, Checked: now, Error: "Image update check interrupted"}
			continue
		}
		hostCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
		snapshot, err := source.Fetch(hostCtx)
		cancel()
		host := HostState{SnapshotVersion: snapshot.Version, Address: source.Address, Checked: now, NodeID: snapshot.NodeID, Hostname: snapshot.Hostname}
		if err != nil || snapshot.Version != management.ImageSnapshotVersion || snapshot.NodeID == "" {
			host.Error = "Agent image metadata unavailable; update agent or check connection"
			state.Hosts[source.ID] = host
			continue
		}
		for _, container := range snapshot.Containers {
			result := Result{ImageContainer: container, Checked: now}
			result.Error = ""
			switch {
			case container.Error != "":
				result.Status = "unknown"
				result.Detail = "Deployed image metadata unavailable"
			case container.Skip != "":
				result.Status = "untracked"
				result.Detail = container.Skip
			case !imageSHA.MatchString(container.ImageID) || !imageSHA.MatchString(container.DeployedDigest()):
				result.Status = "unknown"
				result.Detail = "Invalid deployed image ID"
			case container.ManifestDigest != "" && m.ResolveManifest == nil:
				result.Status, result.Detail = "unknown", "Platform manifest resolver unavailable"
			default:
				if interrupted == nil {
					interrupted = m.workAllowed(ctx)
				}
				if interrupted != nil {
					result.Status, result.Detail = "unknown", "Image update check interrupted"
					host.Results = append(host.Results, result)
					continue
				}
				key := container.Image + "\x00" + container.OS + "\x00" + container.Architecture + "\x00" + container.Variant
				resolve := m.Resolve
				if container.ManifestDigest != "" {
					resolve = m.ResolveManifest
					key += "\x00manifest"
				}
				r, ok := cache[key]
				if !ok {
					domain := registryDomain(container.Image)
					if blocked := throttled[domain]; blocked != nil {
						r.err = blocked
					} else {
						resolveCtx, stop := context.WithTimeout(ctx, 20*time.Second)
						r.digest, r.err = resolve(resolveCtx, container.Image, container.OS, container.Architecture, container.Variant)
						stop()
						var limited RegistryHTTPError
						if errors.As(r.err, &limited) && limited.Status == 429 {
							throttled[domain] = r.err
						}
					}
					cache[key] = r
				}
				if r.err != nil || !imageSHA.MatchString(r.digest) {
					result.Status = "unknown"
					result.Detail = "Registry check failed; verify connection, credentials and platform"
					var status RegistryHTTPError
					if errors.As(r.err, &status) {
						result.Detail = status.Error()
						if status.Status == 429 {
							result.Detail += "; retry at next scheduled check"
						}
					}
				} else {
					result.Available = r.digest
					result.Status = "current"
					if r.digest != container.DeployedDigest() {
						result.Status = "available"
					}
				}
			}
			host.Results = append(host.Results, result)
		}
		state.Hosts[source.ID] = host
	}
	// Only a comparable observed image can resolve history; absence during startup is not proof.
	active := m.events(state, now)
	keys := map[string]bool{}
	for _, event := range active {
		keys[event.ID] = true
	}
	for key, delivery := range state.Sent {
		if selected != nil && !slices.Contains(selected, delivery.HostID) {
			continue
		}
		host, ok := state.Hosts[delivery.HostID]
		known := false
		if ok && host.Error == "" && delivery.Image != "" {
			for _, result := range host.Results {
				if result.Image != delivery.Image {
					continue
				}
				platform := resultPlatform(result)
				if result.Status != "current" && result.Status != "available" && (result.OS == "" || result.Architecture == "" || platform == delivery.Platform || result.Variant == "" && strings.HasPrefix(delivery.Platform, platform+"/")) {
					known = false
					break
				}
				if platform == delivery.Platform && (result.Status == "current" || result.Status == "available") {
					known = true
				}
			}
		}
		if known && !keys[key] {
			delete(state.Sent, key)
		}
	}
	configured := map[string]bool{}
	for _, s := range m.Sources {
		configured[s.ID] = true
	}
	for id := range state.Hosts {
		if !configured[id] {
			delete(state.Hosts, id)
		}
	}
	for key, delivery := range state.Sent {
		if !configured[delivery.HostID] {
			delete(state.Sent, key)
		}
	}
	return errors.Join(m.publish(state), interrupted, ctx.Err())
}

func (m *Monitor) workAllowed(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.Allowed != nil && !m.Allowed() {
		return errors.New("master ownership is not verified; image update work paused")
	}
	return nil
}

func resultPlatform(r Result) string {
	platform := r.OS + "/" + r.Architecture
	if r.Variant != "" {
		platform += "/" + r.Variant
	}
	return platform
}

func eventID(node string, r Result) string {
	key := node + "\x00" + r.Image + "\x00" + resultPlatform(r) + "\x00" + r.DeployedDigest() + "\x00" + r.Available
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

func (m *Monitor) events(state State, now time.Time) []Event {
	groups := map[string]Event{}
	for _, source := range m.Sources {
		host := state.Hosts[source.ID]
		if host.SnapshotVersion != management.ImageSnapshotVersion || host.Address != source.Address || host.Error != "" || host.Checked.IsZero() || now.Sub(host.Checked) > m.Config.Interval+5*time.Minute || now.Before(host.Checked) {
			continue
		}
		for _, r := range host.Results {
			if r.Status != "available" {
				continue
			}
			platform := resultPlatform(r)
			id := eventID(host.NodeID, r)
			e, ok := groups[id]
			if !ok {
				e = Event{ID: id, Type: "image_update_available", Host: HostIdentity{ID: source.ID, Name: source.Name, Address: source.Address, Hostname: host.Hostname}, Image: r.Image, Platform: platform, Deployed: r.DeployedDigest(), Available: r.Available, Detected: r.Checked}
			}
			// Aliases of the same Docker daemon must not duplicate targets or notifications.
			if e.Host.ID != source.ID {
				continue
			}
			e.Targets = append(e.Targets, Target{Container: r.Name, Stack: r.Project, Service: r.Service})
			groups[id] = e
		}
	}
	var events []Event
	for _, e := range groups {
		sort.Slice(e.Targets, func(i, j int) bool { return e.Targets[i].Container < e.Targets[j].Container })
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return events
}

func (m *Monitor) Deliver(ctx context.Context, now time.Time) error {
	m.work.Lock()
	defer m.work.Unlock()
	state := m.copyState()
	// A durable write must work before any external side effect is attempted.
	if err := m.publish(state); err != nil {
		return err
	}
	var failures []error
	events := m.events(state, now)
	if len(events) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	// Reserve half the tick for each channel; SMTP sends one digest, MQTT rotates individual events.
	channelBudget := time.Until(deadline) / 2
	var pending []Event
	for _, event := range events {
		if !state.Sent[event.ID].Email {
			pending = append(pending, event)
		}
	}
	if m.Email != nil && len(pending) != 0 {
		if err := m.workAllowed(ctx); err != nil {
			return err
		}
		channelCtx, stop := context.WithTimeout(ctx, channelBudget)
		err := m.Email(channelCtx, pending)
		stop()
		if err != nil {
			failures = append(failures, errors.New("email notification failed"))
		} else {
			for _, event := range pending {
				delivery := state.Sent[event.ID]
				delivery.HostID, delivery.Image, delivery.Platform = event.Host.ID, event.Image, event.Platform
				delivery.Email = true
				state.Sent[event.ID] = delivery
			}
			// Preserve the whole accepted batch in memory even if saving fails; stop other side effects.
			if err := m.publish(state); err != nil {
				return err
			}
		}
	}
	if m.MQTT == nil {
		return errors.Join(failures...)
	}
	channelCtx, stop := context.WithTimeout(ctx, channelBudget)
	defer stop()
	start := sort.Search(len(events), func(i int) bool { return events[i].ID > m.deliveryAfter })
	for offset := range len(events) {
		if err := m.workAllowed(ctx); err != nil {
			return err
		}
		if channelCtx.Err() != nil {
			failures = append(failures, channelCtx.Err())
			break
		}
		event := events[(start+offset)%len(events)]
		delivery := state.Sent[event.ID]
		if delivery.MQTT {
			continue
		}
		m.deliveryAfter = event.ID
		if err := m.MQTT(channelCtx, event); err != nil {
			failures = append(failures, errors.New("mqtt notification failed"))
			continue
		}
		delivery.HostID, delivery.Image, delivery.Platform = event.Host.ID, event.Image, event.Platform
		delivery.MQTT = true
		state.Sent[event.ID] = delivery
		if err := m.publish(state); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}
