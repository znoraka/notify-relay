package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKeyPEM makes a throwaway P-256 key in the same PKCS8 PEM shape as an
// Apple .p8, so the parsing and signing paths get exercised for real.
func testKeyPEM(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), key
}

func TestParseAPNsKeyAcceptsEscapedNewlines(t *testing.T) {
	pemText, _ := testKeyPEM(t)
	// Coolify env vars can't hold real newlines, so the single-line form with
	// literal \n must parse identically.
	escaped := strings.ReplaceAll(strings.TrimSpace(pemText), "\n", `\n`)
	if _, err := parseAPNsKey(escaped); err != nil {
		t.Fatalf("escaped PEM should parse: %v", err)
	}
	// Coolify stores the value with the backslash escaped, so the container
	// receives \\n. This looks identical in the dashboard and is the difference
	// between a working relay and one that silently disables itself.
	doubleEscaped := strings.ReplaceAll(strings.TrimSpace(pemText), "\n", `\\n`)
	if _, err := parseAPNsKey(doubleEscaped); err != nil {
		t.Fatalf("double-escaped PEM should parse: %v", err)
	}
	if _, err := parseAPNsKey(pemText); err != nil {
		t.Fatalf("plain PEM should parse: %v", err)
	}
	if _, err := parseAPNsKey("not a key"); err == nil {
		t.Fatal("expected non-PEM input to fail")
	}
}

// TestSignAPNsJWTVerifies checks the hand-rolled ES256: Apple rejects the token
// outright if the signature is ASN.1-wrapped instead of the raw r||s pair, and
// that mistake is invisible until a real push fails.
func TestSignAPNsJWTVerifies(t *testing.T) {
	pemText, key := testKeyPEM(t)
	priv, err := parseAPNsKey(pemText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := &t3Config{teamID: "TEAM123", keyID: "KEY456", privateKey: priv}
	now := time.Now()
	token, err := signAPNsJWT(cfg, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d", len(parts))
	}
	var header map[string]string
	decodeSegment(t, parts[0], &header)
	if header["alg"] != "ES256" || header["kid"] != "KEY456" || header["typ"] != "JWT" {
		t.Errorf("header = %+v", header)
	}
	var claims map[string]any
	decodeSegment(t, parts[1], &claims)
	if claims["iss"] != "TEAM123" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if int64(claims["iat"].(float64)) != now.Unix() {
		t.Errorf("iat = %v, want %d", claims["iat"], now.Unix())
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature not base64url: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("ES256 signature must be 64 raw bytes, got %d", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("signature does not verify against the key")
	}
}

func decodeSegment(t *testing.T, segment string, out any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("segment not base64url: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("segment not json: %v", err)
	}
}

func TestAlertAllowedFollowsPreferences(t *testing.T) {
	prefs := t3Prefs{NotifyOnApproval: true, NotifyOnInput: false, NotifyOnCompletion: true, NotifyOnFailure: false}
	cases := map[string]bool{
		"waiting_for_approval": true,
		"waiting_for_input":    false,
		"completed":            true,
		"failed":               false,
		// Progress phases never alert, whatever the switches say.
		"running":  false,
		"starting": false,
		"stale":    false,
	}
	for phase, want := range cases {
		if got := alertAllowed(prefs, phase); got != want {
			t.Errorf("alertAllowed(%q) = %v, want %v", phase, got, want)
		}
	}
}

func TestStatusForPhaseMatchesHostedRelayWording(t *testing.T) {
	cases := map[string]string{
		"waiting_for_approval": "Approval",
		"waiting_for_input":    "Input",
		"completed":            "Done",
		"failed":               "Failed",
		"starting":             "Connecting",
		"running":              "Working",
		"stale":                "Waiting",
	}
	for phase, want := range cases {
		if got := statusForPhase(phase); got != want {
			t.Errorf("statusForPhase(%q) = %q, want %q", phase, got, want)
		}
	}
}

func newTestRelay(t *testing.T) *t3Relay {
	t.Helper()
	pemText, _ := testKeyPEM(t)
	priv, err := parseAPNsKey(pemText)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := &t3Config{
		teamID:     "TEAM",
		keyID:      "KEY",
		privateKey: priv,
		bundleID:   "dev.example.app",
		production: true,
		envSecret:  "env-secret",
		statePath:  t.TempDir() + "/devices.json",
	}
	return newT3Relay(cfg)
}

// TestShouldPushDedupesRepublishes covers the noise case: the environment
// republishes a thread on every projection change, not only on phase changes.
func TestShouldPushDedupesRepublishes(t *testing.T) {
	r := newTestRelay(t)
	if !r.store.shouldPush("env/thread", "waiting_for_input") {
		t.Fatal("first publish of a phase should notify")
	}
	if r.store.shouldPush("env/thread", "waiting_for_input") {
		t.Error("repeat of the same phase should not notify again")
	}
	if !r.store.shouldPush("env/thread", "running") {
		t.Error("a real transition should notify")
	}
	// A tombstone clears the memory so a returning thread can notify again.
	r.store.forgetThread("env/thread")
	if !r.store.shouldPush("env/thread", "running") {
		t.Error("after a tombstone the phase should be notifiable again")
	}
}

func TestDeviceRegistrationRoundTrip(t *testing.T) {
	r := newTestRelay(t)

	body := `{"deviceId":"dev-1","label":"iPhone","platform":"ios","iosMajorVersion":18,
		"pushToken":"abc123","bundleId":"dev.example.app","apsEnvironment":"production",
		"preferences":{"liveActivitiesEnabled":false,"notificationsEnabled":true,
		"notifyOnApproval":true,"notifyOnInput":true,"notifyOnCompletion":false,"notifyOnFailure":true}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/mobile/devices", strings.NewReader(body))
	req.Header.Set("Authorization", "DPoP some-token")
	rec := httptest.NewRecorder()
	r.devices(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, body %s", rec.Code, rec.Body)
	}

	stored := r.store.list()
	if len(stored) != 1 || stored[0].PushToken != "abc123" || !stored[0].Prefs.NotifyOnApproval {
		t.Fatalf("unexpected stored device: %+v", stored)
	}

	// A fresh store over the same path must see the registration, since the app
	// only re-registers on launch/foreground.
	if restored := newT3Store(r.cfg); len(restored.list()) != 1 {
		t.Errorf("expected registration to survive a restart, got %d", len(restored.list()))
	}

	del := httptest.NewRequest(http.MethodDelete, "/v1/mobile/devices/dev-1", nil)
	del.Header.Set("Authorization", "DPoP some-token")
	delRec := httptest.NewRecorder()
	r.devices(delRec, del)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", delRec.Code)
	}
	if len(r.store.list()) != 0 {
		t.Error("device should be gone after DELETE")
	}
}

func TestMobileRoutesRequireAuthorization(t *testing.T) {
	r := newTestRelay(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/mobile/devices", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	r.devices(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated register = %d, want 401", rec.Code)
	}
}

func TestPublishRequiresEnvironmentCredential(t *testing.T) {
	r := newTestRelay(t)
	req := httptest.NewRequest(http.MethodPost,
		"/v1/environments/env-1/threads/thread-1/agent-activity", strings.NewReader(`{"state":null,"proof":"x"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	r.publish(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("publish with a bad credential = %d, want 401", rec.Code)
	}
}

// TestTokenExchangeShape guards the fields the Effect client decodes; a wrong
// literal here fails the app's registration with a schema error, not a 4xx.
func TestTokenExchangeShape(t *testing.T) {
	r := newTestRelay(t)
	form := strings.NewReader("grant_type=urn:ietf:params:oauth:grant-type:token-exchange" +
		"&subject_token=whatever&scope=mobile:registration&client_id=t3-mobile")
	req := httptest.NewRequest(http.MethodPost, "/v1/client/dpop-token", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.token(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("token response not json: %v", err)
	}
	if out["token_type"] != "DPoP" {
		t.Errorf("token_type = %v, want DPoP", out["token_type"])
	}
	if out["issued_token_type"] != "urn:ietf:params:oauth:token-type:access_token" {
		t.Errorf("issued_token_type = %v", out["issued_token_type"])
	}
	// The client compares granted scopes against what it requested.
	if out["scope"] != "mobile:registration" {
		t.Errorf("scope = %v, want the requested scope echoed", out["scope"])
	}
	if tok, _ := out["access_token"].(string); tok == "" {
		t.Error("access_token must be non-empty")
	}
	if exp, _ := out["expires_in"].(float64); exp <= 0 {
		t.Error("expires_in must be positive")
	}
}

// TestDeliverSkipsStaleTerminalStates: reconnects replay old terminal states,
// and a "Done" for a thread that finished an hour ago is noise.
func TestDeliverSkipsStaleTerminalStates(t *testing.T) {
	r := newTestRelay(t)
	r.store.put(t3Device{
		DeviceID:  "dev-1",
		PushToken: "token",
		Prefs:     t3Prefs{NotificationsEnabled: true, NotifyOnCompletion: true},
	})
	stale := t3State{
		EnvironmentID: "env", ThreadID: "thread", Phase: "completed",
		ThreadTitle: "T", ProjectTitle: "P",
		UpdatedAt: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	// No APNs call should happen, so an unreachable Apple host is never contacted.
	if got := r.deliver(stale); len(got) != 0 {
		t.Fatalf("stale terminal state should not deliver, got %+v", got)
	}
}

func TestDeliverSkipsDevicesThatOptedOut(t *testing.T) {
	r := newTestRelay(t)
	r.store.put(t3Device{
		DeviceID:  "no-token",
		PushToken: "",
		Prefs:     t3Prefs{NotificationsEnabled: true, NotifyOnApproval: true},
	})
	r.store.put(t3Device{
		DeviceID:  "notifications-off",
		PushToken: "token",
		Prefs:     t3Prefs{NotificationsEnabled: false, NotifyOnApproval: true},
	})
	r.store.put(t3Device{
		DeviceID:  "approval-off",
		PushToken: "token",
		Prefs:     t3Prefs{NotificationsEnabled: true, NotifyOnApproval: false},
	})
	state := t3State{
		EnvironmentID: "env", ThreadID: "thread", Phase: "waiting_for_approval",
		ThreadTitle: "T", ProjectTitle: "P", UpdatedAt: time.Now().Format(time.RFC3339),
	}
	if got := r.deliver(state); len(got) != 0 {
		t.Fatalf("no device should qualify, got %+v", got)
	}
}

func TestPublishThreadKeyMatchesDeliverKey(t *testing.T) {
	// The tombstone path and deliver() must agree on the key, or a tombstone
	// clears nothing and the thread can never re-notify the same phase.
	key, ok := publishThreadKey("/v1/environments/env-1/threads/thread-1/agent-activity")
	if !ok || key != "env-1/thread-1" {
		t.Fatalf("publishThreadKey = %q, %v; want env-1/thread-1", key, ok)
	}
	// Ids arrive percent-encoded; they must decode back to the values the state
	// body carries.
	key, ok = publishThreadKey("/v1/environments/env%2Fa/threads/thread%20b/agent-activity")
	if !ok || key != "env/a/thread b" {
		t.Errorf("encoded ids: got %q, %v", key, ok)
	}
	for _, bad := range []string{
		"/v1/environments",
		"/v1/environments/env/threads/thread",
		"/v1/environments/env/nope/thread/agent-activity",
		"/other/path",
	} {
		if _, ok := publishThreadKey(bad); ok {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

func TestTombstoneClearsRememberedPhase(t *testing.T) {
	r := newTestRelay(t)
	r.store.shouldPush("env/thread", "running")
	req := httptest.NewRequest(http.MethodPost,
		"/v1/environments/env/threads/thread/agent-activity", strings.NewReader(`{"state":null,"proof":"x"}`))
	req.Header.Set("Authorization", "Bearer env-secret")
	rec := httptest.NewRecorder()
	r.publish(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tombstone status = %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		OK         bool  `json:"ok"`
		Deliveries []any `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("tombstone response not json: %v", err)
	}
	if !out.OK || out.Deliveries == nil {
		t.Errorf("expected ok with an empty (non-null) deliveries array, got %s", rec.Body)
	}
}

func TestTruncateSummary(t *testing.T) {
	if got := truncateSummary("short"); got != "short" {
		t.Errorf("short text should pass through, got %q", got)
	}
	long := strings.Repeat("a", 200)
	got := truncateSummary(long)
	if len([]rune(got)) != maxSummaryText+1 {
		t.Errorf("truncated length = %d runes, want %d plus ellipsis", len([]rune(got)), maxSummaryText)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected an ellipsis suffix, got %q", got)
	}
}
