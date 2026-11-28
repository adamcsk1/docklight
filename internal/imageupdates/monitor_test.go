package imageupdates

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"docklight/internal/management"
)

var oldSHA = "sha256:" + strings.Repeat("a", 64)
var newSHA = "sha256:" + strings.Repeat("b", 64)

func monitorFixture(t *testing.T) (*Monitor, *management.ImageSnapshot, *State) {
	t.Helper()
	snapshot := &management.ImageSnapshot{Version: management.ImageSnapshotVersion, NodeID: "docker-node", Hostname: "nas-main", Containers: []management.ImageContainer{{ID: "one", Name: "web-1", Image: "registry.example/web:latest", ImageID: oldSHA, OS: "linux", Architecture: "amd64", Project: "web", Service: "web"}}}
	stored := &State{}
	m := &Monitor{Config: Config{Interval: time.Hour}, Sources: []Source{{ID: "nas", Name: "Main NAS", Address: "https://nas.example:8081", Fetch: func(context.Context) (management.ImageSnapshot, error) { return *snapshot, nil }}}, Resolve: func(context.Context, string, string, string, string) (string, error) { return newSHA, nil }, Save: func(state State) error {
		b, _ := json.Marshal(state)
		*stored = State{}
		return json.Unmarshal(b, stored)
	}}
	if err := m.Restore(State{}); err != nil {
		t.Fatal(err)
	}
	return m, snapshot, stored
}

func TestMonitorDeployedSHAGroupingAndIndependentDelivery(t *testing.T) {
	m, snapshot, stored := monitorFixture(t)
	replica := snapshot.Containers[0]
	replica.ID = "two"
	replica.Name = "web-2"
	snapshot.Containers = append(snapshot.Containers, replica)
	current := replica
	current.ID = "three"
	current.Name = "web-3"
	current.ImageID = newSHA
	snapshot.Containers = append(snapshot.Containers, current)
	resolves, emails, messages := 0, 0, 0
	m.Resolve = func(context.Context, string, string, string, string) (string, error) { resolves++; return newSHA, nil }
	m.Email = func(_ context.Context, events []Event) error {
		emails++
		if len(events) != 1 {
			t.Fatalf("expected one grouped event, got %d", len(events))
		}
		e := events[0]
		if e.Host.Name != "Main NAS" || e.Host.Hostname != "nas-main" || e.Host.Address != "https://nas.example:8081" || len(e.Targets) != 2 || e.Targets[0].Stack != "web" || e.Targets[0].Service != "web" || e.Deployed != oldSHA || e.Available != newSHA {
			t.Fatalf("incomplete event %+v", e)
		}
		return nil
	}
	m.MQTT = func(context.Context, Event) error {
		messages++
		if messages == 1 {
			return errors.New("offline")
		}
		return nil
	}
	now := time.Now().UTC()
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if resolves != 1 {
		t.Fatalf("same registry image resolved %d times", resolves)
	}
	if got := m.Host("nas", m.Sources[0].Address).Results; got[0].Status != "available" || got[2].Status != "current" {
		t.Fatalf("wrong comparison %+v", got)
	}
	if err := m.Deliver(context.Background(), now); err == nil {
		t.Fatal("MQTT failure hidden")
	}
	if err := m.Deliver(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if emails != 1 || messages != 2 {
		t.Fatalf("deliveries email=%d mqtt=%d", emails, messages)
	}
	restarted, _, _ := monitorFixture(t)
	restarted.Email = m.Email
	restarted.MQTT = m.MQTT
	if err := restarted.Restore(*stored); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Deliver(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if emails != 1 || messages != 2 {
		t.Fatal("restart repeated successful delivery")
	}
	// A completed deployment clears the update; a later different remote SHA creates a new event.
	snapshot.Containers[0].ImageID = newSHA
	snapshot.Containers[1].ImageID = newSHA
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(stored.Sent) != 0 {
		t.Fatal("resolved delivery state not pruned")
	}
}

func TestMonitorUnknownSkippedStaleAndHostReplacement(t *testing.T) {
	for _, failure := range []string{"registry", "agent", "old-agent", "bad-sha", "bad-manifest", "missing-manifest-resolver", "pinned", "local", "metadata"} {
		t.Run(failure, func(t *testing.T) {
			m, snapshot, _ := monitorFixture(t)
			switch failure {
			case "registry":
				m.Resolve = func(context.Context, string, string, string, string) (string, error) {
					return "", errors.New("token=secret")
				}
			case "agent":
				m.Sources[0].Fetch = func(context.Context) (management.ImageSnapshot, error) {
					return management.ImageSnapshot{}, errors.New("offline")
				}
			case "old-agent":
				snapshot.Version = 1
			case "bad-sha":
				snapshot.Containers[0].ImageID = "bad"
			case "bad-manifest":
				snapshot.Containers[0].ManifestDigest = "bad"
			case "missing-manifest-resolver":
				snapshot.Containers[0].ManifestDigest = oldSHA
			case "pinned":
				snapshot.Containers[0].Skip = "Digest-pinned image"
			case "local":
				snapshot.Containers[0].Skip = "Local image"
			case "metadata":
				snapshot.Containers[0].Error = "private upstream error"
			}
			now := time.Now().UTC()
			if err := m.Check(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			m.Email = func(context.Context, []Event) error { t.Fatal("notified unknown or untracked image"); return nil }
			if err := m.Deliver(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(m.copyState())
			if strings.Contains(string(b), "token=secret") || strings.Contains(string(b), "private upstream error") {
				t.Fatal("raw error persisted")
			}
		})
	}
	m, _, _ := monitorFixture(t)
	now := time.Now().UTC()
	_ = m.Check(context.Background(), now)
	m.Email = func(context.Context, []Event) error { t.Fatal("notified stale/moved host"); return nil }
	_ = m.Deliver(context.Background(), now.Add(2*time.Hour))
	m.Sources[0].Address = "https://different-host:8081"
	_ = m.Deliver(context.Background(), now)
	if m.Host("nas", m.Sources[0].Address).Error == "" {
		t.Fatal("old endpoint result reused")
	}
}

func TestMonitorPersistenceFailureAndConcurrentReaders(t *testing.T) {
	m, _, _ := monitorFixture(t)
	now := time.Now().UTC()
	_ = m.Check(context.Background(), now)
	fail := true
	m.Save = func(State) error {
		if fail {
			return errors.New("disk full")
		}
		return nil
	}
	calls := 0
	m.Email = func(context.Context, []Event) error { calls++; fail = true; return nil }
	if err := m.Deliver(context.Background(), now); err == nil || calls != 0 {
		t.Fatal("delivery before durable write")
	}
	fail = false
	if err := m.Deliver(context.Background(), now); err == nil || calls != 1 {
		t.Fatal("post-delivery state failure hidden")
	}
	fail = false
	if err := m.Deliver(context.Background(), now); err != nil || calls != 1 {
		t.Fatal("successful delivery repeated after persistence recovered")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				_ = m.Host("nas", m.Sources[0].Address)
			}
		})
	}
	for range 5 {
		if err := m.Check(context.Background(), now); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestMonitorAliasDedupAndTransientFailureRetainsDelivery(t *testing.T) {
	m, _, _ := monitorFixture(t)
	alias := m.Sources[0]
	alias.ID = "alias"
	m.Sources = append(m.Sources, alias)
	now := time.Now().UTC()
	calls := 0
	m.Email = func(context.Context, []Event) error { calls++; return nil }
	_ = m.Check(context.Background(), now)
	_ = m.Deliver(context.Background(), now)
	if calls != 1 {
		t.Fatal("duplicate daemon alias notification")
	}
	resolve := m.Resolve
	m.Resolve = func(context.Context, string, string, string, string) (string, error) {
		return "", errors.New("offline")
	}
	_ = m.Check(context.Background(), now)
	_ = m.Deliver(context.Background(), now)
	m.Resolve = resolve
	_ = m.Check(context.Background(), now)
	_ = m.Deliver(context.Background(), now)
	if calls != 1 {
		t.Fatal("transient registry failure reset delivery state")
	}
}

func TestRestoreRejectsUnsupportedState(t *testing.T) {
	m := &Monitor{}
	for _, s := range []State{{Version: 2}, {Version: 1}, {Version: 1, Hosts: map[string]HostState{}}} {
		if m.Restore(s) == nil {
			t.Fatal("invalid state accepted")
		}
	}
}

func TestMonitorCancellationAndOwnershipPause(t *testing.T) {
	m, _, stored := monitorFixture(t)
	now := time.Now().UTC()
	_ = m.Check(context.Background(), now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m.Check(ctx, now) == nil || stored.Hosts["nas"].Error == "" {
		t.Fatal("cancelled scan retained healthy status")
	}
	_ = m.Check(context.Background(), now)
	m.Allowed = func() bool { return false }
	m.Email = func(context.Context, []Event) error { t.Fatal("unverified master sent email"); return nil }
	if m.Deliver(context.Background(), now) == nil {
		t.Fatal("ownership pause hidden")
	}
}

func TestMonitorRateLimitStopsRemainingRegistryChecks(t *testing.T) {
	m, snapshot, _ := monitorFixture(t)
	second := snapshot.Containers[0]
	second.ID = "two"
	second.Image = "registry.example/other:latest"
	snapshot.Containers = append(snapshot.Containers, second)
	calls := 0
	m.Resolve = func(context.Context, string, string, string, string) (string, error) {
		calls++
		return "", RegistryHTTPError{Status: 429}
	}
	if err := m.Check(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("rate-limited registry hit again for another image")
	}
	for _, result := range m.Host("nas", m.Sources[0].Address).Results {
		if result.Status != "unknown" || !strings.Contains(result.Detail, "HTTP 429") {
			t.Fatalf("rate limit concealed %+v", result)
		}
	}
}

func TestMonitorStopsScanWhenOwnershipLost(t *testing.T) {
	for _, stage := range []string{"before-fetch", "after-fetch", "after-resolve"} {
		t.Run(stage, func(t *testing.T) {
			m, snapshot, _ := monitorFixture(t)
			second := snapshot.Containers[0]
			second.ID = "two"
			second.Image = "registry.example/other:latest"
			snapshot.Containers = append(snapshot.Containers, second)
			alias := m.Sources[0]
			alias.ID = "alias"
			m.Sources = append(m.Sources, alias)
			allowed := stage != "before-fetch"
			m.Allowed = func() bool { return allowed }
			fetches, resolves := 0, 0
			for i := range m.Sources {
				m.Sources[i].Fetch = func(context.Context) (management.ImageSnapshot, error) {
					fetches++
					if stage == "after-fetch" {
						allowed = false
					}
					return *snapshot, nil
				}
			}
			m.Resolve = func(context.Context, string, string, string, string) (string, error) {
				resolves++
				allowed = false
				return newSHA, nil
			}
			if err := m.Check(context.Background(), time.Now().UTC()); err == nil {
				t.Fatal("ownership loss hidden")
			}
			wantFetches, wantResolves := 1, 0
			if stage == "before-fetch" {
				wantFetches = 0
			}
			if stage == "after-resolve" {
				wantResolves = 1
			}
			if fetches != wantFetches || resolves != wantResolves {
				t.Fatalf("work continued after ownership loss: fetch=%d resolve=%d", fetches, resolves)
			}
		})
	}
}

func TestMonitorDeliveryChannelsAndEventsMakeIndependentProgress(t *testing.T) {
	m, snapshot, _ := monitorFixture(t)
	for _, name := range []string{"second", "third"} {
		c := snapshot.Containers[0]
		c.ID, c.Name, c.Image = name, name, "registry.example/"+name+":latest"
		snapshot.Containers = append(snapshot.Containers, c)
	}
	now := time.Now().UTC()
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	emails, messages := map[string]int{}, map[string]int{}
	emailCalls := 0
	m.Email = func(ctx context.Context, events []Event) error {
		emailCalls++
		if len(events) != 3 {
			t.Fatalf("incomplete digest: %d events", len(events))
		}
		for _, e := range events {
			emails[e.ID]++
		}
		<-ctx.Done()
		return ctx.Err()
	}
	m.MQTT = func(ctx context.Context, e Event) error {
		if ctx.Err() != nil {
			t.Fatal("SMTP consumed MQTT budget")
		}
		messages[e.ID]++
		return nil
	}
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := m.Deliver(ctx, now); err == nil {
			t.Fatal("email failure hidden")
		}
		cancel()
	}
	if emailCalls != 3 || len(emails) != 3 || len(messages) != 3 {
		t.Fatalf("starved events: email=%v mqtt=%v", emails, messages)
	}
	for _, count := range messages {
		if count != 1 {
			t.Fatal("successful MQTT delivery repeated")
		}
	}
}

func TestMonitorPrunesResolvedImageDespiteUnrelatedUnknown(t *testing.T) {
	m, snapshot, stored := monitorFixture(t)
	broken := snapshot.Containers[0]
	broken.ID = "broken"
	broken.Image = "registry.example/broken:latest"
	broken.Error = "Unavailable"
	snapshot.Containers = append(snapshot.Containers, broken)
	now := time.Now().UTC()
	calls := 0
	m.Email = func(context.Context, []Event) error { calls++; return nil }
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := m.Deliver(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	// Backfill identity for persisted delivery records written by the earlier implementation.
	for id, delivery := range stored.Sent {
		delivery.Image, delivery.Platform = "", ""
		stored.Sent[id] = delivery
	}
	if err := m.Restore(*stored); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range m.copyState().Sent {
		if delivery.Image == "" || delivery.Platform == "" {
			t.Fatal("legacy image identity not recovered")
		}
	}
	snapshot.Containers[0].Error = "Unavailable"
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(stored.Sent) != 1 {
		t.Fatal("unknown monitored image lost delivery history")
	}
	snapshot.Containers[0].Error = ""
	snapshot.Containers[0].ImageID = newSHA
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(stored.Sent) != 0 {
		t.Fatal("unrelated unknown image retained resolved delivery")
	}
	snapshot.Containers[0].ImageID = oldSHA
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := m.Deliver(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("later recurrence of resolved update suppressed")
	}
}

func TestMonitorPruningSeparatesPlatforms(t *testing.T) {
	for _, unknownPlatform := range []string{"other-architecture", "other-variant", "missing-variant", "missing-metadata"} {
		t.Run(unknownPlatform, func(t *testing.T) {
			m, snapshot, stored := monitorFixture(t)
			snapshot.Containers[0].Architecture, snapshot.Containers[0].Variant = "arm", "v7"
			now := time.Now().UTC()
			m.Email = func(context.Context, []Event) error { return nil }
			if err := m.Check(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if err := m.Deliver(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			broken := snapshot.Containers[0]
			broken.ID, broken.Error = "unknown", "Unavailable"
			switch unknownPlatform {
			case "other-architecture":
				broken.Architecture, broken.Variant = "amd64", ""
			case "other-variant":
				broken.Variant = "v6"
			case "missing-variant":
				broken.Variant = ""
			case "missing-metadata":
				broken.OS, broken.Architecture, broken.Variant = "", "", ""
			}
			snapshot.Containers[0].ImageID = newSHA
			snapshot.Containers = append(snapshot.Containers, broken)
			if err := m.Check(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			want := 0
			if unknownPlatform == "missing-variant" || unknownPlatform == "missing-metadata" {
				want = 1
			}
			if len(stored.Sent) != want {
				t.Fatalf("incorrect platform cleanup: %d records, want %d", len(stored.Sent), want)
			}
		})
	}
}

func TestMonitorDeliveryStopsAfterOwnershipLoss(t *testing.T) {
	m, _, stored := monitorFixture(t)
	now := time.Now().UTC()
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	allowed := true
	m.Allowed = func() bool { return allowed }
	m.Email = func(context.Context, []Event) error { allowed = false; return nil }
	m.MQTT = func(context.Context, Event) error { t.Fatal("notification sent after ownership loss"); return nil }
	if err := m.Deliver(context.Background(), now); err == nil {
		t.Fatal("ownership loss hidden")
	}
	for _, delivery := range stored.Sent {
		if !delivery.Email || delivery.MQTT {
			t.Fatal("acknowledged email not preserved")
		}
	}
}

func TestMonitorDigestAcrossHostsRetriesAndRestarts(t *testing.T) {
	m, snapshot, stored := monitorFixture(t)
	other := *snapshot
	other.NodeID, other.Hostname = "other-node", "other-host"
	other.Containers = append([]management.ImageContainer(nil), snapshot.Containers...)
	other.Containers[0].Image = "registry.example/other:latest"
	m.Sources = append(m.Sources, Source{ID: "other", Name: "Other host", Address: "https://other.example:8081", Fetch: func(context.Context) (management.ImageSnapshot, error) { return other, nil }})
	third := snapshot.Containers[0]
	third.ID, third.Image = "third", "registry.example/third:latest"
	snapshot.Containers = append(snapshot.Containers, third)
	now := time.Now().UTC()
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	events := m.events(m.copyState(), now)
	// Already delivered email must not appear in a later digest, even while MQTT is pending.
	state := m.copyState()
	state.Sent[events[0].ID] = Delivery{HostID: events[0].Host.ID, Image: events[0].Image, Platform: events[0].Platform, Email: true}
	if err := m.Restore(state); err != nil {
		t.Fatal(err)
	}
	emails, messages, fail := 0, 0, true
	m.Email = func(_ context.Context, batch []Event) error {
		emails++
		if len(batch) != 2 || batch[0].ID == events[0].ID || batch[1].ID == events[0].ID {
			t.Fatalf("wrong pending batch: %+v", batch)
		}
		if fail {
			return errors.New("SMTP unavailable")
		}
		return nil
	}
	m.MQTT = func(context.Context, Event) error { messages++; return nil }
	if err := m.Deliver(context.Background(), now); err == nil {
		t.Fatal("SMTP failure hidden")
	}
	for _, event := range events[1:] {
		if stored.Sent[event.ID].Email || !stored.Sent[event.ID].MQTT {
			t.Fatal("failed email acknowledged or independent MQTT blocked")
		}
	}
	fail = false
	if err := m.Deliver(context.Background(), now); err != nil || emails != 2 || messages != 3 {
		t.Fatalf("batch retry repeated successful channel: emails=%d mqtt=%d err=%v", emails, messages, err)
	}
	for _, event := range events {
		if !stored.Sent[event.ID].Email || !stored.Sent[event.ID].MQTT {
			t.Fatal("batch acknowledgement incomplete")
		}
	}
	if err := m.Restore(*stored); err != nil {
		t.Fatal(err)
	}
	if err := m.Deliver(context.Background(), now); err != nil || emails != 2 || messages != 3 {
		t.Fatal("restart repeated acknowledged digest", err)
	}
	// A subsequent cycle sends one fleet digest for the newly changed registry content.
	m.Resolve = func(context.Context, string, string, string, string) (string, error) {
		return "sha256:" + strings.Repeat("c", 64), nil
	}
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	m.Email = func(_ context.Context, batch []Event) error {
		emails++
		hosts := map[string]bool{}
		for _, e := range batch {
			hosts[e.Host.ID] = true
		}
		if len(batch) != 3 || len(hosts) != 2 {
			t.Fatal("fleet digest omitted updates or hosts")
		}
		return nil
	}
	if err := m.Deliver(context.Background(), now); err != nil || emails != 3 || messages != 6 {
		t.Fatal("new fleet cycle not delivered once", err)
	}
}

func TestMonitorDigestPersistenceFailureKeepsWholeBatch(t *testing.T) {
	m, snapshot, _ := monitorFixture(t)
	second := snapshot.Containers[0]
	second.ID, second.Image = "second", "registry.example/second:latest"
	snapshot.Containers = append(snapshot.Containers, second)
	now := time.Now().UTC()
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	fail, emails, saves := false, 0, 0
	m.Save = func(state State) error {
		saves++
		if fail {
			for _, event := range m.events(state, now) {
				if !state.Sent[event.ID].Email {
					t.Fatal("partially acknowledged batch persisted")
				}
			}
			return errors.New("disk full")
		}
		return nil
	}
	m.Email = func(_ context.Context, batch []Event) error {
		emails++
		if len(batch) != 2 {
			t.Fatal("batch split")
		}
		fail = true
		return nil
	}
	m.MQTT = func(context.Context, Event) error { t.Fatal("side effect after failed persistence"); return nil }
	if err := m.Deliver(context.Background(), now); err == nil || emails != 1 || saves != 2 {
		t.Fatal("batch persistence failure not handled", err)
	}
	fail = false
	m.MQTT = nil
	if err := m.Deliver(context.Background(), now); err != nil || emails != 1 {
		t.Fatal("accepted batch repeated after persistence recovery", err)
	}
}

func TestMonitorMQTTRotationSurvivesSlowEvent(t *testing.T) {
	m, snapshot, _ := monitorFixture(t)
	for _, name := range []string{"second", "third"} {
		c := snapshot.Containers[0]
		c.ID, c.Image = name, "registry.example/"+name+":latest"
		snapshot.Containers = append(snapshot.Containers, c)
	}
	now := time.Now().UTC()
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	m.MQTT = func(ctx context.Context, e Event) error {
		seen[e.ID] = true
		<-ctx.Done()
		return ctx.Err()
	}
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if err := m.Deliver(ctx, now); err == nil {
			t.Fatal("MQTT timeout hidden")
		}
		cancel()
	}
	if len(seen) != 3 {
		t.Fatal("slow MQTT event starved later events")
	}
}

func TestMonitorRestartRetainsDeliveryAcrossInventoryGaps(t *testing.T) {
	for _, gap := range []string{"empty", "other-image", "untracked", "unknown", "offline"} {
		t.Run(gap, func(t *testing.T) {
			m, snapshot, stored := monitorFixture(t)
			now := time.Now().UTC()
			emails, messages := 0, 0
			m.Email = func(context.Context, []Event) error { emails++; return nil }
			m.MQTT = func(context.Context, Event) error {
				messages++
				if messages == 1 {
					return errors.New("broker offline")
				}
				return nil
			}
			if err := m.Check(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if err := m.Deliver(context.Background(), now); err == nil {
				t.Fatal("MQTT failure hidden")
			}
			original, fetch := snapshot.Containers, m.Sources[0].Fetch
			missing := original[0]
			switch gap {
			case "empty":
				snapshot.Containers = nil
			case "other-image":
				missing.Image, missing.ImageID = "registry.example/other:latest", newSHA
				snapshot.Containers = []management.ImageContainer{missing}
			case "untracked":
				missing.Skip = "Disabled by container label"
				snapshot.Containers = []management.ImageContainer{missing}
			case "unknown":
				missing.Error = "Image metadata unavailable"
				snapshot.Containers = []management.ImageContainer{missing}
			case "offline":
				m.Sources[0].Fetch = func(context.Context) (management.ImageSnapshot, error) {
					return management.ImageSnapshot{}, errors.New("offline")
				}
			}
			if err := m.Check(context.Background(), now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(stored.Sent) != 1 {
				t.Fatal("inventory gap discarded notification history")
			}
			for _, delivery := range stored.Sent {
				if !delivery.Email || delivery.MQTT {
					t.Fatal("inventory gap changed independent channel acknowledgements")
				}
			}
			restarted, _, _ := monitorFixture(t)
			if err := restarted.Restore(*stored); err != nil {
				t.Fatal(err)
			}
			snapshot.Containers = original
			restarted.Sources[0].Fetch = fetch
			restarted.Email, restarted.MQTT = m.Email, m.MQTT
			if err := restarted.Check(context.Background(), now.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := restarted.Deliver(context.Background(), now.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if emails != 1 || messages != 2 || restarted.Host("nas", m.Sources[0].Address).Results[0].Status != "available" {
				t.Fatalf("restart repeated email, lost retry or hid real update: email=%d mqtt=%d", emails, messages)
			}
		})
	}
}

func TestMonitorUntrackedReplicaDoesNotResolveHistory(t *testing.T) {
	m, snapshot, stored := monitorFixture(t)
	now := time.Now().UTC()
	m.Email = func(context.Context, []Event) error { return nil }
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := m.Deliver(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	current := snapshot.Containers[0]
	current.ID, current.Name, current.ImageID = "current", "web-2", newSHA
	snapshot.Containers[0].Skip = "Disabled by container label"
	snapshot.Containers = append(snapshot.Containers, current)
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(stored.Sent) != 1 {
		t.Fatal("current replica discarded untracked replica delivery history")
	}
}

func TestMonitorSeparatesClassicAndManifestDigests(t *testing.T) {
	m, snapshot, _ := monitorFixture(t)
	snapshot.Containers[0].ImageID = newSHA
	containerd := snapshot.Containers[0]
	manifest := "sha256:" + strings.Repeat("c", 64)
	containerd.ID, containerd.Name, containerd.ImageID, containerd.ManifestDigest = "containerd", "containerd", "sha256:"+strings.Repeat("d", 64), manifest
	snapshot.Containers = append(snapshot.Containers, containerd)
	configs, manifests := 0, 0
	m.Resolve = func(context.Context, string, string, string, string) (string, error) { configs++; return newSHA, nil }
	m.ResolveManifest = func(context.Context, string, string, string, string) (string, error) {
		manifests++
		return manifest, nil
	}
	if err := m.Check(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, result := range m.Host("nas", m.Sources[0].Address).Results {
		if result.Status != "current" {
			t.Fatalf("compared different digest kinds: %+v", result)
		}
	}
	if configs != 1 || manifests != 1 {
		t.Fatal("different digest kinds shared registry cache")
	}
}

func TestMonitorContainerdRestartUsesPlatformIdentity(t *testing.T) {
	m, snapshot, stored := monitorFixture(t)
	snapshot.Containers[0].ManifestDigest = oldSHA
	snapshot.Containers[0].ImageID = "sha256:" + strings.Repeat("c", 64)
	m.Resolve = func(context.Context, string, string, string, string) (string, error) {
		t.Fatal("containerd used config digest")
		return "", nil
	}
	m.ResolveManifest = func(context.Context, string, string, string, string) (string, error) { return newSHA, nil }
	emails := 0
	m.Email = func(_ context.Context, events []Event) error {
		emails++
		if len(events) != 1 || events[0].Deployed != oldSHA || events[0].Available != newSHA {
			t.Fatal("notification mixed index and manifest digests")
		}
		return nil
	}
	now := time.Now().UTC()
	if err := m.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := m.Deliver(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	restarted, _, _ := monitorFixture(t)
	if err := restarted.Restore(*stored); err != nil {
		t.Fatal(err)
	}
	// Index identity can change while this container still runs the same platform manifest.
	snapshot.Containers[0].ImageID = "sha256:" + strings.Repeat("d", 64)
	restarted.Sources, restarted.Resolve, restarted.ResolveManifest, restarted.Email = m.Sources, m.Resolve, m.ResolveManifest, m.Email
	if err := restarted.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Deliver(context.Background(), now); err != nil || emails != 1 {
		t.Fatal("index identity change repeated platform notification", err)
	}
	snapshot.Containers[0].ManifestDigest = newSHA
	if err := restarted.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if result := restarted.Host("nas", m.Sources[0].Address).Results[0]; result.Status != "current" {
		t.Fatalf("matching platform manifest marked outdated: %+v", result)
	}
	if err := restarted.Deliver(context.Background(), now); err != nil || emails != 1 {
		t.Fatal("matching manifest triggered email", err)
	}
}

func TestMonitorReplacementNodeHasIndependentHistory(t *testing.T) {
	m, snapshot, _ := monitorFixture(t)
	emails := 0
	m.Email = func(context.Context, []Event) error { emails++; return nil }
	now := time.Now().UTC()
	for _, node := range []string{snapshot.NodeID, "replacement-node"} {
		snapshot.NodeID = node
		if err := m.Check(context.Background(), now); err != nil {
			t.Fatal(err)
		}
		if err := m.Deliver(context.Background(), now); err != nil {
			t.Fatal(err)
		}
	}
	if emails != 2 {
		t.Fatal("replacement node silently reused another node's acknowledgements")
	}
}
