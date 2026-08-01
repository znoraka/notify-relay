// t3.go — a minimal T3 Code relay bolted onto notify-relay.
//
// T3 Code Mobile only shows its "Device Notifications" switch as on once a
// relay has accepted this device's registration, and the environment server
// only pushes agent activity to a relay it holds a credential for. The hosted
// relay that normally plays that part is a Cloudflare Worker backed by
// Postgres, Clerk and Cloudflare Queues — far more machinery than one person
// with one phone needs.
//
// So this is the smallest server that satisfies both ends of that contract:
// the mobile app registers its APNs token here, the environment publishes
// thread state here, and this file turns the second into an APNs push at the
// first. Response shapes must match packages/contracts/src/relay.ts exactly —
// the clients decode with Effect Schema and reject anything that doesn't fit.
//
// Deliberately NOT implemented, because a single-user relay does not need it:
// DPoP proofs are accepted and ignored rather than verified, the environment's
// signed publish proof is accepted and ignored, and there is no Clerk. Auth is
// one shared secret for the environment plus a bearer token the app obtains
// from the token endpoint. Anyone holding those can push notifications to one
// phone; that is the whole blast radius.
//
// Live Activities are stubbed (registration accepted, nothing sent) so the app
// can call them without erroring. Alerts are the whole of v1.
package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---- configuration ----

type t3Config struct {
	teamID     string
	keyID      string
	privateKey *ecdsa.PrivateKey
	bundleID   string // default APNs topic when a registration omits its own
	production bool   // api.push.apple.com vs api.sandbox.push.apple.com
	envSecret  string // bearer the environment presents when publishing
	statePath  string
}

// loadT3Config reads the APNs credentials. Returns nil (with a reason) when the
// relay should run without the T3 routes — the Signal half must keep working on
// a box that has no Apple key configured.
func loadT3Config() (*t3Config, string) {
	team, key := os.Getenv("APNS_TEAM_ID"), os.Getenv("APNS_KEY_ID")
	pemText, bundle := os.Getenv("APNS_PRIVATE_KEY"), os.Getenv("APNS_BUNDLE_ID")
	if team == "" || key == "" || pemText == "" || bundle == "" {
		return nil, "APNS_TEAM_ID/APNS_KEY_ID/APNS_PRIVATE_KEY/APNS_BUNDLE_ID not all set"
	}
	priv, err := parseAPNsKey(pemText)
	if err != nil {
		return nil, "APNS_PRIVATE_KEY unusable: " + err.Error()
	}
	secret := os.Getenv("T3_ENV_CREDENTIAL")
	if secret == "" {
		return nil, "T3_ENV_CREDENTIAL not set"
	}
	return &t3Config{
		teamID:     team,
		keyID:      key,
		privateKey: priv,
		bundleID:   bundle,
		// Ad-hoc, TestFlight and App Store builds all use the production APNs
		// host; only a development-signed (debug) build gets sandbox tokens.
		production: envOr("APNS_ENVIRONMENT", "production") != "sandbox",
		envSecret:  secret,
		statePath:  envOr("T3_STATE", "/var/lib/notify-relay/t3-devices.json"),
	}, ""
}

// parseAPNsKey accepts the .p8 Apple hands out. Coolify env vars can't hold real
// newlines, so a single-line value with literal \n is also accepted.
func parseAPNsKey(text string) (*ecdsa.PrivateKey, error) {
	text = strings.ReplaceAll(strings.TrimSpace(text), `\n`, "\n")
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, fmt.Errorf("not PEM (expected -----BEGIN PRIVATE KEY-----)")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ECDSA key (%T)", parsed)
	}
	return key, nil
}

// ---- device registry ----

// t3Prefs mirrors RelayAgentAwarenessPreferences: the per-event switches the
// app writes on every registration.
type t3Prefs struct {
	LiveActivitiesEnabled bool `json:"liveActivitiesEnabled"`
	NotificationsEnabled  bool `json:"notificationsEnabled"`
	NotifyOnApproval      bool `json:"notifyOnApproval"`
	NotifyOnInput         bool `json:"notifyOnInput"`
	NotifyOnCompletion    bool `json:"notifyOnCompletion"`
	NotifyOnFailure       bool `json:"notifyOnFailure"`
}

type t3Device struct {
	DeviceID       string  `json:"deviceId"`
	Label          string  `json:"label"`
	Platform       string  `json:"platform"`
	IosMajorVerion int     `json:"iosMajorVersion"`
	AppVersion     string  `json:"appVersion,omitempty"`
	BundleID       string  `json:"bundleId,omitempty"`
	ApsEnvironment string  `json:"apsEnvironment,omitempty"`
	PushToken      string  `json:"pushToken,omitempty"`
	Prefs          t3Prefs `json:"preferences"`
	UpdatedAt      string  `json:"updatedAt"`
}

type t3Store struct {
	cfg *t3Config

	mu      sync.Mutex
	devices map[string]t3Device
	// lastPhase records the phase most recently pushed per thread, so a burst of
	// republishes (the environment re-publishes on every projection change, not
	// only on phase changes) sends one notification per real transition.
	lastPhase map[string]string
}

func newT3Store(cfg *t3Config) *t3Store {
	s := &t3Store{cfg: cfg, devices: map[string]t3Device{}, lastPhase: map[string]string{}}
	s.load()
	return s
}

// load restores registrations from disk. A missing or corrupt file is not fatal:
// the app re-registers on launch and on every return to the foreground, so the
// worst case is one missed notification before it heals itself.
func (s *t3Store) load() {
	b, err := os.ReadFile(s.cfg.statePath)
	if err != nil {
		return
	}
	var devices map[string]t3Device
	if err := json.Unmarshal(b, &devices); err != nil {
		log.Printf("t3: state unreadable, starting empty: %v", err)
		return
	}
	s.devices = devices
	log.Printf("t3: restored %d device(s)", len(devices))
}

// save persists via write-to-temp-then-rename so a crash mid-write cannot leave
// a truncated file that load would then discard.
func (s *t3Store) save() {
	if err := os.MkdirAll(filepath.Dir(s.cfg.statePath), 0o755); err != nil {
		log.Printf("t3: state dir: %v", err)
		return
	}
	b, err := json.MarshalIndent(s.devices, "", "  ")
	if err != nil {
		return
	}
	tmp := s.cfg.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("t3: state write: %v", err)
		return
	}
	if err := os.Rename(tmp, s.cfg.statePath); err != nil {
		log.Printf("t3: state rename: %v", err)
	}
}

func (s *t3Store) put(d t3Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[d.DeviceID] = d
	s.save()
}

func (s *t3Store) remove(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, deviceID)
	s.save()
}

func (s *t3Store) list() []t3Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]t3Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	return out
}

// dropToken forgets a push token APNs has rejected as permanently invalid, so a
// stale registration stops being retried on every publish.
func (s *t3Store) dropToken(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.devices[deviceID]; ok {
		d.PushToken = ""
		s.devices[deviceID] = d
		s.save()
	}
}

// shouldPush reports whether this phase is a new transition for the thread, and
// records it. Publishes that repeat the current phase are dropped.
func (s *t3Store) shouldPush(threadKey, phase string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPhase[threadKey] == phase {
		return false
	}
	s.lastPhase[threadKey] = phase
	return true
}

func (s *t3Store) forgetThread(threadKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lastPhase, threadKey)
}

// ---- APNs ----

// apnsAuth caches the provider JWT. Apple rejects tokens refreshed more often
// than once per 20 minutes and expires them at 60, so refresh in between.
type apnsAuth struct {
	mu       sync.Mutex
	token    string
	issuedAt time.Time
}

const apnsTokenTTL = 45 * time.Minute

func (a *apnsAuth) bearer(cfg *t3Config) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Since(a.issuedAt) < apnsTokenTTL {
		return a.token, nil
	}
	tok, err := signAPNsJWT(cfg, time.Now())
	if err != nil {
		return "", err
	}
	a.token, a.issuedAt = tok, time.Now()
	return tok, nil
}

// signAPNsJWT builds the ES256 provider token by hand — the payload is three
// claims, and hand-rolling it keeps this service dependency-free.
func signAPNsJWT(cfg *t3Config, now time.Time) (string, error) {
	header := map[string]string{"alg": "ES256", "kid": cfg.keyID, "typ": "JWT"}
	claims := map[string]any{"iss": cfg.teamID, "iat": now.Unix()}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64url(hb) + "." + b64url(cb)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, cfg.privateKey, digest[:])
	if err != nil {
		return "", err
	}
	// JWS wants the raw r||s pair, each left-padded to the curve size — not the
	// ASN.1 wrapping ecdsa.SignASN1 would produce.
	size := (cfg.privateKey.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 2*size)
	padInto(sig[:size], r)
	padInto(sig[size:], s)
	return signing + "." + b64url(sig), nil
}

func padInto(dst []byte, n *big.Int) {
	b := n.Bytes()
	copy(dst[len(dst)-len(b):], b)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// apnsClient talks HTTP/2 to Apple. Go's default transport negotiates h2 over
// ALPN for https, which is all APNs requires.
var apnsClient = &http.Client{Timeout: 15 * time.Second}

type apnsResult struct {
	ok     bool
	status int
	reason string
	id     string
	// gone marks a token Apple says will never work again, so we stop keeping it.
	gone bool
}

func (r *t3Relay) sendAPNs(device t3Device, payload map[string]any) apnsResult {
	bearer, err := r.auth.bearer(r.cfg)
	if err != nil {
		log.Printf("t3: apns token: %v", err)
		return apnsResult{reason: "provider_token"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return apnsResult{reason: "encode"}
	}

	host := "api.push.apple.com"
	// A registration reports the environment it was signed for; trust it over the
	// service-wide default, since a dev build and a preview build can be
	// installed side by side and their tokens are not interchangeable.
	sandbox := !r.cfg.production
	if device.ApsEnvironment == "sandbox" {
		sandbox = true
	} else if device.ApsEnvironment == "production" {
		sandbox = false
	}
	if sandbox {
		host = "api.sandbox.push.apple.com"
	}
	topic := device.BundleID
	if topic == "" {
		topic = r.cfg.bundleID
	}

	req, err := http.NewRequest(http.MethodPost, "https://"+host+"/3/device/"+device.PushToken, strings.NewReader(string(body)))
	if err != nil {
		return apnsResult{reason: "request"}
	}
	req.Header.Set("authorization", "bearer "+bearer)
	req.Header.Set("apns-topic", topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")

	resp, err := apnsClient.Do(req)
	if err != nil {
		log.Printf("t3: apns send: %v", err)
		return apnsResult{reason: "network"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	id := resp.Header.Get("apns-id")
	if resp.StatusCode == http.StatusOK {
		return apnsResult{ok: true, status: resp.StatusCode, id: id}
	}
	var apnsErr struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &apnsErr)
	log.Printf("t3: apns %d %s (device %s)", resp.StatusCode, apnsErr.Reason, device.DeviceID)
	return apnsResult{
		status: resp.StatusCode,
		reason: apnsErr.Reason,
		id:     id,
		gone:   apnsErr.Reason == "BadDeviceToken" || apnsErr.Reason == "Unregistered",
	}
}

// ---- relay ----

type t3Relay struct {
	cfg   *t3Config
	store *t3Store
	auth  *apnsAuth
}

func newT3Relay(cfg *t3Config) *t3Relay {
	return &t3Relay{cfg: cfg, store: newT3Store(cfg), auth: &apnsAuth{}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// clientAuthed accepts any non-empty DPoP/Bearer authorization. The token was
// minted by this service and there is exactly one user; the proof header the
// app also sends is deliberately not verified.
func clientAuthed(req *http.Request) bool {
	h := req.Header.Get("Authorization")
	for _, scheme := range []string{"DPoP ", "Bearer "} {
		if rest, ok := strings.CutPrefix(h, scheme); ok && strings.TrimSpace(rest) != "" {
			return true
		}
	}
	return false
}

func (r *t3Relay) envAuthed(req *http.Request) bool {
	token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	return ok && strings.TrimSpace(token) == r.cfg.envSecret
}

// token implements the OAuth token exchange the app bootstraps with. The
// subject token would be a Clerk JWT against the hosted relay; here anything
// non-empty is accepted and the returned token is opaque.
func (r *t3Relay) token(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := req.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	scope := req.PostFormValue("scope")
	if scope == "" {
		scope = "mobile:registration"
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "entropy", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":      b64url(raw),
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type":        "DPoP",
		"expires_in":        3600,
		// Echo the requested scopes: the client compares what it asked for
		// against what it got and errors on a mismatch.
		"scope": scope,
	})
}

func (r *t3Relay) devices(w http.ResponseWriter, req *http.Request) {
	if !clientAuthed(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch req.Method {
	case http.MethodPost:
		var in t3Device
		if err := json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(in.DeviceID) == "" {
			http.Error(w, "deviceId required", http.StatusBadRequest)
			return
		}
		in.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		r.store.put(in)
		log.Printf("t3: registered device %s (%s), push token %s, notifications=%v",
			in.DeviceID, in.Label, presence(in.PushToken), in.Prefs.NotificationsEnabled)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		deviceID := strings.TrimPrefix(req.URL.Path, "/v1/mobile/devices/")
		r.store.remove(deviceID)
		log.Printf("t3: unregistered device %s", deviceID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
	}
}

func presence(s string) string {
	if s == "" {
		return "absent"
	}
	return "present"
}

// liveActivities accepts registrations so the app's toggle does not error, but
// v1 sends no Live Activity pushes. The app treats a successful registration as
// "armed", so leaving this on will show a card that never updates — keep the
// Live Activity Updates switch off until v2 implements the pushes.
func (r *t3Relay) liveActivities(w http.ResponseWriter, req *http.Request) {
	if !clientAuthed(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	io.Copy(io.Discard, io.LimitReader(req.Body, 64<<10))
	log.Printf("t3: live activity registration accepted (not implemented in v1)")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// agentActivitySnapshot lets the app decide whether arming a Live Activity is
// worthwhile. v1 never has an aggregate, so it always says "nothing running".
func (r *t3Relay) agentActivitySnapshot(w http.ResponseWriter, req *http.Request) {
	if !clientAuthed(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aggregate": nil})
}

func (r *t3Relay) listEnvironments(w http.ResponseWriter, req *http.Request) {
	if !clientAuthed(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Environments are linked by writing credentials straight into the server's
	// secret store, so this relay never learns about them. The app only uses
	// this list for T3 Connect's remote-environment picker, which is not part of
	// the notification path.
	writeJSON(w, http.StatusOK, map[string]any{"environments": []any{}})
}

func (r *t3Relay) listDevices(w http.ResponseWriter, req *http.Request) {
	if !clientAuthed(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	devices := r.store.list()
	out := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		appVersion := any(nil)
		if d.AppVersion != "" {
			appVersion = d.AppVersion
		}
		out = append(out, map[string]any{
			"deviceId":        d.DeviceID,
			"label":           d.Label,
			"platform":        d.Platform,
			"iosMajorVersion": d.IosMajorVerion,
			"appVersion":      appVersion,
			"notifications": map[string]any{
				"enabled":            d.Prefs.NotificationsEnabled,
				"notifyOnApproval":   d.Prefs.NotifyOnApproval,
				"notifyOnInput":      d.Prefs.NotifyOnInput,
				"notifyOnCompletion": d.Prefs.NotifyOnCompletion,
				"notifyOnFailure":    d.Prefs.NotifyOnFailure,
			},
			"liveActivities": map[string]any{"enabled": d.Prefs.LiveActivitiesEnabled},
			"updatedAt":      d.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// t3State mirrors RelayAgentActivityState.
type t3State struct {
	EnvironmentID string `json:"environmentId"`
	ThreadID      string `json:"threadId"`
	ProjectTitle  string `json:"projectTitle"`
	ThreadTitle   string `json:"threadTitle"`
	Phase         string `json:"phase"`
	Headline      string `json:"headline"`
	Detail        string `json:"detail,omitempty"`
	ModelTitle    string `json:"modelTitle"`
	UpdatedAt     string `json:"updatedAt"`
	DeepLink      string `json:"deepLink"`
}

// statusForPhase mirrors the hosted relay's wording so a thread reads the same
// on the lock screen as it does in the sidebar.
func statusForPhase(phase string) string {
	switch phase {
	case "waiting_for_approval":
		return "Approval"
	case "waiting_for_input":
		return "Input"
	case "completed":
		return "Done"
	case "failed":
		return "Failed"
	case "starting":
		return "Connecting"
	case "running":
		return "Working"
	case "stale":
		return "Waiting"
	}
	return phase
}

func alertAllowed(p t3Prefs, phase string) bool {
	switch phase {
	case "waiting_for_approval":
		return p.NotifyOnApproval
	case "waiting_for_input":
		return p.NotifyOnInput
	case "completed":
		return p.NotifyOnCompletion
	case "failed":
		return p.NotifyOnFailure
	}
	return false
}

// terminalFreshness bounds how late a Done/Failed alert may arrive. Replays of
// old terminal states are common on reconnect and a notification about a thread
// that finished an hour ago is noise.
const terminalFreshness = 2 * time.Minute

const maxSummaryText = 120

func truncateSummary(s string) string {
	if len(s) <= maxSummaryText {
		return s
	}
	return s[:maxSummaryText] + "…"
}

// publishThreadKey extracts "<environmentId>/<threadId>" from
// /v1/environments/{environmentId}/threads/{threadId}/agent-activity, matching
// the key deliver() remembers phases under. Ids are percent-encoded by the
// client, so they are decoded back to the values the state body carries.
func publishThreadKey(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/v1/environments/")
	if !ok {
		return "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 4 || parts[1] != "threads" || parts[3] != "agent-activity" {
		return "", false
	}
	environmentID, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", false
	}
	threadID, err := url.PathUnescape(parts[2])
	if err != nil {
		return "", false
	}
	if environmentID == "" || threadID == "" {
		return "", false
	}
	return environmentID + "/" + threadID, true
}

// publish is the environment-facing endpoint: it accepts one thread's current
// state and fans an alert out to every registered device that wants it.
func (r *t3Relay) publish(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !r.envAuthed(req) {
		log.Printf("t3: publish rejected, bad environment credential")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in struct {
		State *t3State `json:"state"`
		Proof string   `json:"proof"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, 256<<10)).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// A null state is a tombstone: the thread is gone or no longer interesting.
	// Nothing to notify, but the remembered phase must go so the thread can
	// notify again if it comes back. The ids come from the path here, since
	// there is no state body to read them from.
	if in.State == nil {
		if key, ok := publishThreadKey(req.URL.Path); ok {
			r.store.forgetThread(key)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deliveries": []any{}})
		return
	}

	deliveries := r.deliver(*in.State)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deliveries": deliveries})
}

func (r *t3Relay) deliver(state t3State) []map[string]any {
	deliveries := []map[string]any{}

	threadKey := state.EnvironmentID + "/" + state.ThreadID
	if !r.store.shouldPush(threadKey, state.Phase) {
		return deliveries
	}
	// Done/Failed for a thread that settled a while ago is a replay, not news.
	if state.Phase == "completed" || state.Phase == "failed" {
		if at, err := time.Parse(time.RFC3339, state.UpdatedAt); err == nil {
			if time.Since(at) > terminalFreshness {
				return deliveries
			}
		}
	}

	title := truncateSummary(state.ThreadTitle)
	body := truncateSummary(statusForPhase(state.Phase) + ": " + state.ProjectTitle)

	for _, device := range r.store.list() {
		if device.PushToken == "" || !device.Prefs.NotificationsEnabled {
			continue
		}
		if !alertAllowed(device.Prefs, state.Phase) {
			continue
		}
		payload := map[string]any{
			"aps": map[string]any{
				"alert":     map[string]any{"title": title, "body": body},
				"sound":     "default",
				"thread-id": threadKey,
			},
			// The app routes a tap from these: deepLink is preferred, and the
			// explicit ids are the fallback it also accepts.
			"deepLink":      state.DeepLink,
			"environmentId": state.EnvironmentID,
			"threadId":      state.ThreadID,
			"phase":         state.Phase,
			"updatedAt":     state.UpdatedAt,
		}
		res := r.sendAPNs(device, payload)
		if res.gone {
			r.store.dropToken(device.DeviceID)
		}
		delivery := map[string]any{
			"deviceId":   device.DeviceID,
			"kind":       "push_notification",
			"ok":         res.ok,
			"apnsStatus": nilIfZero(res.status),
			"apnsReason": nilIfEmpty(res.reason),
			"apnsId":     nilIfEmpty(res.id),
		}
		deliveries = append(deliveries, delivery)
		if res.ok {
			log.Printf("t3: pushed %q to %s (%s)", body, device.DeviceID, state.Phase)
		}
	}
	return deliveries
}

func nilIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (r *t3Relay) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "relay"})
}

func (r *t3Relay) authorizationServerMetadata(w http.ResponseWriter, req *http.Request) {
	issuer := originOf(req)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                issuer,
		"token_endpoint":                        issuer + "/v1/client/dpop-token",
		"grant_types_supported":                 []string{"urn:ietf:params:oauth:grant-type:token-exchange"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"dpop_signing_alg_values_supported":     []string{"ES256"},
		"scopes_supported":                      []string{"environment:connect", "environment:status", "mobile:registration"},
	})
}

func (r *t3Relay) protectedResourceMetadata(w http.ResponseWriter, req *http.Request) {
	issuer := originOf(req)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                          issuer,
		"authorization_servers":             []string{issuer},
		"scopes_supported":                  []string{"environment:connect", "environment:status", "mobile:registration"},
		"dpop_bound_access_tokens_required": true,
		"dpop_signing_alg_values_supported": []string{"ES256"},
	})
}

// originOf reconstructs the public origin. Behind Coolify's proxy the request
// URL has no scheme or host, so the forwarded headers are the only source.
func originOf(req *http.Request) string {
	scheme := req.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "https"
	}
	host := req.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = req.Host
	}
	return scheme + "://" + host
}

// registerT3Routes mounts the relay. Returns false when APNs is not configured,
// leaving the Signal webhook relay to run on its own.
func registerT3Routes(mux *http.ServeMux) bool {
	cfg, reason := loadT3Config()
	if cfg == nil {
		log.Printf("t3 relay disabled: %s", reason)
		return false
	}
	r := newT3Relay(cfg)

	mux.HandleFunc("/health", r.health)
	mux.HandleFunc("/.well-known/oauth-authorization-server", r.authorizationServerMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource", r.protectedResourceMetadata)
	mux.HandleFunc("/v1/client/dpop-token", r.token)
	mux.HandleFunc("/v1/client/devices", r.listDevices)
	mux.HandleFunc("/v1/environments", r.listEnvironments)
	mux.HandleFunc("/v1/mobile/devices", r.devices)
	mux.HandleFunc("/v1/mobile/devices/", r.devices)
	mux.HandleFunc("/v1/mobile/live-activities", r.liveActivities)
	mux.HandleFunc("/v1/mobile/agent-activity", r.agentActivitySnapshot)
	// Publish carries ids in the path: /v1/environments/<env>/threads/<thread>/agent-activity
	mux.HandleFunc("/v1/environments/", r.publish)

	env := "production"
	if !cfg.production {
		env = "sandbox"
	}
	log.Printf("t3 relay enabled: topic %s, APNs %s, state %s", cfg.bundleID, env, cfg.statePath)
	return true
}
