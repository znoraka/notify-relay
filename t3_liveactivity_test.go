package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func stateAt(threadID, phase string, age time.Duration) t3State {
	return t3State{
		EnvironmentID: "env",
		ThreadID:      threadID,
		ProjectTitle:  "proj",
		ThreadTitle:   "thread " + threadID,
		ModelTitle:    "claude",
		Phase:         phase,
		Headline:      "headline",
		UpdatedAt:     time.Now().Add(-age).UTC().Format(time.RFC3339),
		DeepLink:      "/threads/env/" + threadID,
	}
}

func TestAggregateCountsOnlyLiveWork(t *testing.T) {
	now := time.Now()
	agg := buildAggregate([]t3State{
		stateAt("a", "running", time.Minute),
		stateAt("b", "waiting_for_approval", time.Minute),
		stateAt("c", "completed", time.Minute),
	}, now)
	if agg == nil {
		t.Fatal("expected an aggregate")
	}
	if agg.ActiveCount != 2 {
		t.Errorf("activeCount = %d, want 2 (terminal rows do not count)", agg.ActiveCount)
	}
	// The finished thread still rides along so a completion is visible.
	if len(agg.Activities) != 3 {
		t.Errorf("activities = %d, want 3", len(agg.Activities))
	}
	if agg.Subtitle != "Agent work in progress" {
		t.Errorf("subtitle = %q", agg.Subtitle)
	}
}

func TestAggregateNilWhenNothingToShow(t *testing.T) {
	now := time.Now()
	if agg := buildAggregate(nil, now); agg != nil {
		t.Errorf("no rows should produce no card, got %+v", agg)
	}
	// A long-finished thread should not keep a card alive.
	if agg := buildAggregate([]t3State{stateAt("a", "completed", time.Hour)}, now); agg != nil {
		t.Errorf("stale terminal row should produce no card, got %+v", agg)
	}
}

// TestAggregateExpiresAbandonedRows covers an environment that dies mid-run: it
// never publishes a terminal state, so without a TTL its threads inflate
// activeCount forever.
func TestAggregateExpiresAbandonedRows(t *testing.T) {
	now := time.Now()
	if agg := buildAggregate([]t3State{stateAt("a", "running", 3*time.Hour)}, now); agg != nil {
		t.Errorf("a running row older than its TTL should drop out, got %+v", agg)
	}
	// A thread waiting on a human legitimately sits for hours.
	agg := buildAggregate([]t3State{stateAt("a", "waiting_for_input", 3*time.Hour)}, now)
	if agg == nil || agg.ActiveCount != 1 {
		t.Errorf("a waiting row should survive well past the running TTL, got %+v", agg)
	}
}

func TestAggregateTerminalOnlyReportsOutcome(t *testing.T) {
	agg := buildAggregate([]t3State{stateAt("a", "failed", time.Minute)}, time.Now())
	if agg == nil {
		t.Fatal("a recent failure should still paint the card")
	}
	if agg.ActiveCount != 0 || agg.Subtitle != "Agent work failed" {
		t.Errorf("got activeCount=%d subtitle=%q", agg.ActiveCount, agg.Subtitle)
	}
}

func TestAggregateCapsRows(t *testing.T) {
	rows := []t3State{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		rows = append(rows, stateAt(id, "running", time.Minute))
	}
	agg := buildAggregate(rows, time.Now())
	if len(agg.Activities) != maxActivityRows {
		t.Errorf("activities = %d, want the display cap %d", len(agg.Activities), maxActivityRows)
	}
	// The count reports reality even when the list is truncated.
	if agg.ActiveCount != 7 {
		t.Errorf("activeCount = %d, want 7", agg.ActiveCount)
	}
}

// TestContentStateShape pins the encoding the iOS widget decodes. props is a
// JSON *string*, not a nested object; getting this wrong yields a push Apple
// accepts and a card that never renders.
func TestContentStateShape(t *testing.T) {
	agg := buildAggregate([]t3State{stateAt("a", "running", time.Minute)}, time.Now())
	cs := contentState(agg)
	if cs["name"] != liveActivityName {
		t.Errorf("name = %v, want %q", cs["name"], liveActivityName)
	}
	props, ok := cs["props"].(string)
	if !ok {
		t.Fatalf("props must be a JSON string, got %T", cs["props"])
	}
	var decoded t3Aggregate
	if err := json.Unmarshal([]byte(props), &decoded); err != nil {
		t.Fatalf("props is not valid JSON: %v", err)
	}
	if decoded.ActiveCount != 1 || len(decoded.Activities) != 1 {
		t.Errorf("round-tripped aggregate lost content: %+v", decoded)
	}
	if decoded.Activities[0].Status != "Working" {
		t.Errorf("status = %q, want the sidebar wording", decoded.Activities[0].Status)
	}
}

func TestAggregateSanitisesDeepLink(t *testing.T) {
	state := stateAt("a", "running", time.Minute)
	state.DeepLink = "https://evil.example/whatever"
	agg := buildAggregate([]t3State{state}, time.Now())
	if got := agg.Activities[0].DeepLink; got != "/" {
		t.Errorf("deepLink = %q, want it rejected down to \"/\"", got)
	}
}

// TestActivityTokenSurvivesReRegistration: the app re-registers on every launch
// with a payload that has no activity token. Losing it would orphan a running
// card — it could never be updated or ended.
func TestActivityTokenSurvivesReRegistration(t *testing.T) {
	r := newTestRelay(t)
	r.store.put(t3Device{DeviceID: "dev-1", PushToken: "tok"})
	r.store.setActivityToken("dev-1", "activity-tok")

	device, _ := r.store.get("dev-1")
	if device.ActivityToken != "activity-tok" {
		t.Fatalf("setup failed: %+v", device)
	}

	// Simulate what the devices handler does on a re-register.
	incoming := t3Device{DeviceID: "dev-1", PushToken: "tok"}
	if previous, ok := r.store.get(incoming.DeviceID); ok && incoming.ActivityToken == "" {
		incoming.ActivityToken = previous.ActivityToken
	}
	r.store.put(incoming)

	device, _ = r.store.get("dev-1")
	if device.ActivityToken != "activity-tok" {
		t.Errorf("activity token lost on re-registration: %+v", device)
	}
}

func TestStoreRowsSurviveRestart(t *testing.T) {
	r := newTestRelay(t)
	r.store.putRow("env/a", stateAt("a", "running", time.Minute))
	r.store.put(t3Device{DeviceID: "dev-1", PushToken: "tok"})

	restored := newT3Store(r.cfg)
	if len(restored.rowList()) != 1 {
		t.Errorf("rows lost across restart: %d", len(restored.rowList()))
	}
	if len(restored.list()) != 1 {
		t.Errorf("devices lost across restart: %d", len(restored.list()))
	}
}

func TestCardThrottleSendsFirstThenCoalesces(t *testing.T) {
	var mu sync.Mutex
	var sent []*t3Aggregate
	throttle := newCardThrottle(50*time.Millisecond, func(_ string, agg *t3Aggregate) apnsResult {
		mu.Lock()
		sent = append(sent, agg)
		mu.Unlock()
		return apnsResult{ok: true}
	})

	first := &t3Aggregate{ActiveCount: 1}
	if _, queued := throttle.submit("dev-1", first); queued {
		t.Fatal("the first update should go out immediately")
	}

	// A burst inside the window must not each become a push.
	for i := 2; i <= 5; i++ {
		if _, queued := throttle.submit("dev-1", &t3Aggregate{ActiveCount: i}); !queued {
			t.Fatalf("update %d should have been coalesced", i)
		}
	}

	mu.Lock()
	count := len(sent)
	mu.Unlock()
	if count != 1 {
		t.Fatalf("sent %d pushes during the burst, want 1", count)
	}

	// The trailing send carries the newest state, not an intermediate one.
	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("sent %d pushes total, want 2 (one immediate, one trailing)", len(sent))
	}
	if sent[1].ActiveCount != 5 {
		t.Errorf("trailing send carried activeCount %d, want the newest (5)", sent[1].ActiveCount)
	}
}

// TestCardThrottleResetDropsPending: an alerting update or a start has just
// gone out, so a queued routine redraw is both stale and redundant.
func TestCardThrottleResetDropsPending(t *testing.T) {
	var mu sync.Mutex
	sends := 0
	throttle := newCardThrottle(50*time.Millisecond, func(_ string, _ *t3Aggregate) apnsResult {
		mu.Lock()
		sends++
		mu.Unlock()
		return apnsResult{ok: true}
	})

	throttle.submit("dev-1", &t3Aggregate{ActiveCount: 1}) // immediate
	throttle.submit("dev-1", &t3Aggregate{ActiveCount: 2}) // queued
	throttle.reset("dev-1")

	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if sends != 1 {
		t.Errorf("sends = %d, want 1 — reset should have dropped the queued redraw", sends)
	}
}

func TestCardThrottleIsPerDevice(t *testing.T) {
	var mu sync.Mutex
	sends := map[string]int{}
	throttle := newCardThrottle(time.Minute, func(deviceID string, _ *t3Aggregate) apnsResult {
		mu.Lock()
		sends[deviceID]++
		mu.Unlock()
		return apnsResult{ok: true}
	})

	// One device's burst must not starve another device's first update.
	throttle.submit("dev-1", &t3Aggregate{})
	throttle.submit("dev-1", &t3Aggregate{})
	throttle.submit("dev-2", &t3Aggregate{})

	mu.Lock()
	defer mu.Unlock()
	if sends["dev-1"] != 1 || sends["dev-2"] != 1 {
		t.Errorf("sends = %v, want one each", sends)
	}
}

// TestPruneDropsUndisplayableRows: rows are only removed on a tombstone, and an
// environment that is switched off never sends one. Linking one machine
// replayed 242 threads, nearly all long finished — without pruning the state
// file grows without bound and every publish re-serialises all of it.
func TestPruneDropsUndisplayableRows(t *testing.T) {
	r := newTestRelay(t)
	r.store.putRow("env/live", stateAt("live", "running", time.Minute))
	r.store.putRow("env/waiting", stateAt("waiting", "waiting_for_input", 3*time.Hour))
	r.store.putRow("env/just-done", stateAt("just-done", "completed", time.Minute))
	r.store.putRow("env/long-done", stateAt("long-done", "completed", 2*time.Hour))
	r.store.putRow("env/abandoned", stateAt("abandoned", "running", 3*time.Hour))

	kept := map[string]bool{}
	for _, row := range r.store.rowList() {
		kept[row.ThreadID] = true
	}
	if !kept["live"] || !kept["waiting"] || !kept["just-done"] {
		t.Errorf("displayable rows were pruned: %v", kept)
	}
	if kept["long-done"] || kept["abandoned"] {
		t.Errorf("undisplayable rows survived: %v", kept)
	}
}
