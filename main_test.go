package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFormatPlandropPreview(t *testing.T) {
	body := []byte(`{"event":"created","title":"My Plan","url":"https://plans.example/p/abc",
		"machine":"laptop","image":"https://plans.example/p/abc/og.png","description":"a nice plan"}`)
	msg := format(body)
	if !strings.Contains(msg.text, "https://plans.example/p/abc") {
		t.Fatalf("body should carry the url for Signal to match: %q", msg.text)
	}
	if msg.preview == nil {
		t.Fatal("expected a preview when image is present")
	}
	if msg.preview.url != "https://plans.example/p/abc" {
		t.Errorf("preview.url = %q", msg.preview.url)
	}
	if msg.preview.title != "My Plan" {
		t.Errorf("preview.title = %q", msg.preview.title)
	}
	if msg.preview.description != "a nice plan" {
		t.Errorf("preview.description = %q", msg.preview.description)
	}
	if msg.preview.image != "https://plans.example/p/abc/og.png" {
		t.Errorf("preview.image = %q", msg.preview.image)
	}
}

func TestFormatNoImageNoPreview(t *testing.T) {
	// Older senders omit image/description — must stay backward compatible.
	body := []byte(`{"event":"done","title":"Old Plan","url":"https://plans.example/p/xyz"}`)
	msg := format(body)
	if msg.preview != nil {
		t.Fatalf("expected no preview without an image, got %+v", msg.preview)
	}
}

func TestFormatMessagePassthrough(t *testing.T) {
	msg := format([]byte(`{"message":"hello"}`))
	if msg.text != "hello" || msg.preview != nil {
		t.Fatalf("got text=%q preview=%v", msg.text, msg.preview)
	}
}

func TestSendAttachesLinkPreview(t *testing.T) {
	imgBytes := []byte("\x89PNG\r\n\x1a\nfake-png-data")
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(imgBytes)
	}))
	defer img.Close()

	var got map[string]any
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer sidecar.Close()

	r := &relay{signalURL: sidecar.URL, number: "+15551234567", rl: map[string]*limiter{}}
	r.send("plandrop", message{
		text: "📋 New plan: My Plan\nhttps://plans.example/p/abc",
		preview: &preview{
			url:         "https://plans.example/p/abc",
			title:       "My Plan",
			description: "a nice plan",
			image:       img.URL,
		},
	})

	lp, ok := got["link_preview"].(map[string]any)
	if !ok {
		t.Fatalf("no link_preview in payload: %v", got)
	}
	if lp["url"] != "https://plans.example/p/abc" || lp["title"] != "My Plan" || lp["description"] != "a nice plan" {
		t.Errorf("link_preview fields wrong: %v", lp)
	}
	want := base64.StdEncoding.EncodeToString(imgBytes)
	if lp["base64_thumbnail"] != want {
		t.Errorf("base64_thumbnail = %q, want %q", lp["base64_thumbnail"], want)
	}
}

func TestSendImageFailFallsBackToText(t *testing.T) {
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer img.Close()

	var got map[string]any
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer sidecar.Close()

	r := &relay{signalURL: sidecar.URL, number: "+15551234567", rl: map[string]*limiter{}}
	r.send("plandrop", message{
		text:    "text body\nhttps://plans.example/p/abc",
		preview: &preview{url: "https://plans.example/p/abc", title: "t", image: img.URL},
	})

	if _, present := got["link_preview"]; present {
		t.Errorf("image fetch failed: expected text-only, but link_preview was sent: %v", got)
	}
	if got["message"] != "text body\nhttps://plans.example/p/abc" {
		t.Errorf("message body not delivered: %v", got["message"])
	}
}
