// t3_liveactivity.go — the Live Activity half of the T3 relay.
//
// An alert is one notification about one thread. A Live Activity is a
// persistent lock-screen card showing every agent at once, so it needs
// something an alert does not: state. The relay has to remember the latest
// published state per thread, fold those into one aggregate, and push the
// aggregate to the card on every change.
//
// Shapes here mirror the hosted relay exactly (infra/relay/src/agentActivity)
// because the iOS widget decodes them: content-state is
// {name, props: "<json>"} where props is the serialised aggregate, and the
// widget is registered under the name "AgentActivity". Guessing any of this
// produces a push Apple accepts and a card that never renders.
package main

import (
	"encoding/json"
	"log"
	"sort"
	"strings"
	"time"
)

// liveActivityName must match the widget registered by expo-widgets in
// apps/mobile/src/widgets/AgentActivity.tsx.
const liveActivityName = "AgentActivity"

// attributesType is the Swift ActivityAttributes type expo-widgets generates.
const attributesType = "LiveActivityAttributes"

const (
	// A card that stops being updated should grey out rather than lie.
	staleAfter = 10 * time.Minute
	// How long an ended card lingers before iOS clears it.
	dismissAfter = 5 * time.Minute
	// An end with no final content has nothing worth reading, so it goes fast.
	contentlessDismissAfter = 15 * time.Second

	// Rows are only removed when an environment publishes a terminal state. An
	// environment that dies mid-run never does, so without a cutoff its threads
	// inflate activeCount forever. Running work expires quickly; a thread
	// waiting on a human can legitimately sit for hours.
	runningRowTTL = 2 * time.Hour
	waitingRowTTL = 24 * time.Hour
	// How long a finished thread keeps its Done/Failed row while others run.
	terminalDisplayTTL = 15 * time.Minute

	// The lock-screen banner renders five rows; the expanded Dynamic Island
	// shows the top three of those.
	maxActivityRows = 5
	maxStatusText   = 40
)

// t3AggregateRow mirrors RelayAgentActivityAggregateRow.
type t3AggregateRow struct {
	EnvironmentID string `json:"environmentId"`
	ThreadID      string `json:"threadId"`
	ProjectTitle  string `json:"projectTitle"`
	ThreadTitle   string `json:"threadTitle"`
	ModelTitle    string `json:"modelTitle"`
	Phase         string `json:"phase"`
	Status        string `json:"status"`
	UpdatedAt     string `json:"updatedAt"`
	DeepLink      string `json:"deepLink"`
}

// t3Aggregate mirrors RelayAgentActivityAggregateState.
type t3Aggregate struct {
	Title       string           `json:"title"`
	Subtitle    string           `json:"subtitle"`
	ActiveCount int              `json:"activeCount"`
	UpdatedAt   string           `json:"updatedAt"`
	Activities  []t3AggregateRow `json:"activities"`
}

func isTerminalPhase(phase string) bool {
	return phase == "completed" || phase == "failed"
}

func parseUpdatedAt(value string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// isExpired drops rows whose environment stopped publishing. An unparseable
// timestamp counts as expired: it cannot be reasoned about and keeping it would
// pin a row forever.
func isExpired(state t3State, now time.Time) bool {
	at, ok := parseUpdatedAt(state.UpdatedAt)
	if !ok {
		return true
	}
	ttl := waitingRowTTL
	if state.Phase == "running" || state.Phase == "starting" {
		ttl = runningRowTTL
	}
	return now.Sub(at) > ttl
}

func isRecentTerminal(state t3State, now time.Time) bool {
	if !isTerminalPhase(state.Phase) {
		return false
	}
	at, ok := parseUpdatedAt(state.UpdatedAt)
	if !ok {
		return false
	}
	return now.Sub(at) <= terminalDisplayTTL
}

func aggregateRow(state t3State) t3AggregateRow {
	deepLink := strings.TrimSpace(state.DeepLink)
	// The widget treats the link as an in-app path; anything else is not
	// navigable and "/" at least opens the app.
	if !strings.HasPrefix(deepLink, "/") || strings.HasPrefix(deepLink, "//") {
		deepLink = "/"
	}
	return t3AggregateRow{
		EnvironmentID: state.EnvironmentID,
		ThreadID:      state.ThreadID,
		ProjectTitle:  truncateSummary(state.ProjectTitle),
		ThreadTitle:   truncateSummary(state.ThreadTitle),
		ModelTitle:    truncateSummary(state.ModelTitle),
		Phase:         state.Phase,
		Status:        truncateText(statusForPhase(state.Phase), maxStatusText),
		UpdatedAt:     state.UpdatedAt,
		DeepLink:      truncateText(deepLink, 512),
	}
}

func truncateText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return strings.TrimRight(s[:max-3], " ") + "..."
}

// buildAggregate folds every remembered thread into the card's state, or
// returns nil when there is nothing worth showing — which the caller turns into
// an "end" so the card does not sit there empty.
func buildAggregate(rows []t3State, now time.Time) *t3Aggregate {
	var active, recentTerminal []t3State
	for _, row := range rows {
		if isExpired(row, now) {
			continue
		}
		if isTerminalPhase(row.Phase) {
			if isRecentTerminal(row, now) {
				recentTerminal = append(recentTerminal, row)
			}
			continue
		}
		active = append(active, row)
	}

	newestFirst := func(list []t3State) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	}
	newestFirst(active)
	newestFirst(recentTerminal)

	if len(active) == 0 {
		// No live work. Recently finished threads keep the card showing
		// Done/Failed rather than blanking, until they age out.
		if len(recentTerminal) == 0 {
			return nil
		}
		newest := recentTerminal[0]
		subtitle := "Agent work completed"
		if newest.Phase == "failed" {
			subtitle = "Agent work failed"
		}
		return &t3Aggregate{
			Title:       "T3 Code",
			Subtitle:    subtitle,
			ActiveCount: 0,
			UpdatedAt:   newest.UpdatedAt,
			Activities:  rowsFor(recentTerminal),
		}
	}

	// Finished threads ride along after the active ones, display slots
	// permitting, so a completion reads as Done instead of silently vanishing.
	displayed := append(append([]t3State{}, active...), recentTerminal...)
	updatedAt := displayed[0].UpdatedAt
	for _, row := range displayed {
		if row.UpdatedAt > updatedAt {
			updatedAt = row.UpdatedAt
		}
	}
	return &t3Aggregate{
		Title:       "T3 Code",
		Subtitle:    "Agent work in progress",
		ActiveCount: len(active),
		UpdatedAt:   updatedAt,
		Activities:  rowsFor(displayed),
	}
}

func rowsFor(states []t3State) []t3AggregateRow {
	if len(states) > maxActivityRows {
		states = states[:maxActivityRows]
	}
	out := make([]t3AggregateRow, 0, len(states))
	for _, state := range states {
		out = append(out, aggregateRow(state))
	}
	return out
}

// contentState is the payload the widget decodes: the aggregate is handed over
// as a JSON *string* under props, not as a nested object.
func contentState(agg *t3Aggregate) map[string]any {
	encoded, err := json.Marshal(agg)
	if err != nil {
		encoded = []byte("{}")
	}
	return map[string]any{"name": liveActivityName, "props": string(encoded)}
}

type liveActivityAlert struct {
	title string
	body  string
}

// sendLiveActivity pushes one card event. event is start, update or end.
// A start goes to the device's push-to-start token; update and end go to the
// token the app registered for the running activity.
func (r *t3Relay) sendLiveActivity(
	device t3Device,
	token string,
	event string,
	agg *t3Aggregate,
	alert *liveActivityAlert,
) apnsResult {
	now := time.Now()
	timestamp := now.Unix()
	aps := map[string]any{"timestamp": timestamp, "event": event}

	switch event {
	case "end":
		if agg != nil {
			aps["content-state"] = contentState(agg)
			aps["dismissal-date"] = timestamp + int64(dismissAfter.Seconds())
		} else {
			aps["dismissal-date"] = timestamp + int64(contentlessDismissAfter.Seconds())
		}
	default:
		aps["content-state"] = contentState(agg)
		aps["stale-date"] = timestamp + int64(staleAfter.Seconds())
		if event == "start" {
			aps["attributes-type"] = attributesType
			aps["attributes"] = map[string]any{}
			// Asks iOS for the per-activity update token, which the app then
			// registers — without it the card can be started and never updated.
			aps["input-push-token"] = 1
			aps["alert"] = map[string]any{"title": agg.Title, "body": agg.Subtitle}
		}
	}
	// An alert dict on an update makes it an "alerting" update: iOS wakes the
	// screen and plays the haptic instead of silently redrawing.
	if alert != nil && event != "start" {
		aps["alert"] = map[string]any{"title": alert.title, "body": alert.body, "sound": "default"}
	}

	// Routine redraws stay at the budget-friendly priority; anything the user
	// should notice goes out immediately.
	priority := "10"
	if event == "update" && alert == nil {
		priority = "5"
	}

	return r.sendAPNsRequest(apnsRequest{
		device:   device,
		token:    token,
		payload:  map[string]any{"aps": aps},
		pushType: "liveactivity",
		topic:    r.cfg.topicFor(device) + ".push-type.liveactivity",
		priority: priority,
	})
}

// syncLiveActivities brings every registered device's card in line with the
// current aggregate. alert is non-nil when the change is worth waking the
// screen for.
func (r *t3Relay) syncLiveActivities(agg *t3Aggregate, alert *liveActivityAlert) []map[string]any {
	deliveries := []map[string]any{}
	for _, device := range r.store.list() {
		if !device.Prefs.LiveActivitiesEnabled {
			continue
		}

		switch {
		case agg == nil:
			// Nothing left to show. Only a running card can be ended.
			if device.ActivityToken == "" {
				continue
			}
			res := r.sendLiveActivity(device, device.ActivityToken, "end", nil, nil)
			// The activity is over either way; keeping the token would only
			// produce pushes to a card that no longer exists.
			r.store.clearActivityToken(device.DeviceID)
			deliveries = append(deliveries, liveActivityDelivery(device.DeviceID, "live_activity_end", res))

		case device.ActivityToken != "":
			res := r.sendLiveActivity(device, device.ActivityToken, "update", agg, alert)
			if res.gone {
				r.store.clearActivityToken(device.DeviceID)
			}
			deliveries = append(deliveries, liveActivityDelivery(device.DeviceID, "live_activity_update", res))

		case device.PushToStartToken != "":
			res := r.sendLiveActivity(device, device.PushToStartToken, "start", agg, nil)
			deliveries = append(deliveries, liveActivityDelivery(device.DeviceID, "live_activity_start", res))
		}
	}
	return deliveries
}

func liveActivityDelivery(deviceID, kind string, res apnsResult) map[string]any {
	if res.ok {
		log.Printf("t3: %s ok for %s", kind, deviceID)
	}
	return map[string]any{
		"deviceId":   deviceID,
		"kind":       kind,
		"ok":         res.ok,
		"apnsStatus": nilIfZero(res.status),
		"apnsReason": nilIfEmpty(res.reason),
		"apnsId":     nilIfEmpty(res.id),
	}
}
