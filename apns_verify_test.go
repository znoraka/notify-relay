package main

import (
	"os"
	"strings"
	"testing"
)

// TestVerifyAPNsCredentials is a manual credential check, skipped unless the
// key is supplied. It exercises the real sendAPNs path against Apple with a
// deliberately invalid device token:
//
//	BadDeviceToken       -> key, team and topic are all correct
//	InvalidProviderToken -> the key or team is wrong
//	TopicDisallowed      -> the topic is not covered by this key
//
// Run with:
//
//	APNS_VERIFY_KEY=~/Downloads/AuthKey_XXXX.p8 APNS_KEY_ID=... \
//	APNS_TEAM_ID=... APNS_BUNDLE_ID=... go test -run TestVerifyAPNsCredentials -v
func TestVerifyAPNsCredentials(t *testing.T) {
	keyPath := os.Getenv("APNS_VERIFY_KEY")
	if keyPath == "" {
		t.Skip("APNS_VERIFY_KEY not set")
	}
	pemText, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	priv, err := parseAPNsKey(string(pemText))
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	cfg := &t3Config{
		teamID:     os.Getenv("APNS_TEAM_ID"),
		keyID:      os.Getenv("APNS_KEY_ID"),
		privateKey: priv,
		bundleID:   os.Getenv("APNS_BUNDLE_ID"),
		production: true,
		envSecret:  "unused",
		statePath:  t.TempDir() + "/devices.json",
	}
	relay := &t3Relay{cfg: cfg, store: newT3Store(cfg), auth: &apnsAuth{}}

	device := t3Device{DeviceID: "verify", PushToken: strings.Repeat("ab", 32)}
	res := relay.sendAPNs(device, map[string]any{
		"aps": map[string]any{"alert": map[string]any{"title": "credential check", "body": "never arrives"}},
	})

	t.Logf("status=%d reason=%q apns-id=%q", res.status, res.reason, res.id)
	switch res.reason {
	case "BadDeviceToken":
		t.Log("PASS: key, team and topic all accepted by Apple")
	case "InvalidProviderToken", "ExpiredProviderToken":
		t.Fatal("FAIL: Apple rejected the provider token — key ID or team ID is wrong")
	case "TopicDisallowed":
		t.Fatal("FAIL: the topic is not covered by this key")
	default:
		t.Fatalf("unexpected reason %q (status %d)", res.reason, res.status)
	}
}
