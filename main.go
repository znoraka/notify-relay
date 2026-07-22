// notify-relay — a small webhook-to-Signal relay. Projects POST JSON to
// /hook/<source> with a per-source bearer token; the relay formats a message
// and delivers it to a signal-cli-rest-api sidecar as Note-to-Self. No queue,
// no persistence: a missed notification is annoying, not critical.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type sourceCfg struct {
	Token   string `json:"token"`
	Prefix  string `json:"prefix"`
	Enabled bool   `json:"enabled"`
}

type config struct {
	Sources map[string]sourceCfg `json:"sources"`
}

func loadConfig() (config, error) {
	path := envOr("CONFIG", "/etc/notify-relay/sources.json")
	var cfg config
	if b, err := os.ReadFile(path); err == nil {
		return cfg, json.Unmarshal(b, &cfg)
	}
	if j := os.Getenv("SOURCES_JSON"); j != "" {
		return cfg, json.Unmarshal([]byte(j), &cfg)
	}
	return cfg, fmt.Errorf("no config: %s missing and SOURCES_JSON unset", path)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type relay struct {
	cfg       config
	signalURL string

	mu     sync.Mutex
	number string // discovered from the sidecar, cached

	rlMu sync.Mutex
	rl   map[string]*limiter
}

// limiter enforces one message per interval per source; excess messages
// coalesce into a single trailing send carrying the latest text + (+N more).
type limiter struct {
	last    time.Time
	pending message
	extra   int
	timer   bool
}

// message is a formatted notification: text plus an optional Signal link
// preview. Signal fetches no previews recipient-side, so the sender must
// attach the card; preview is populated only when the payload carries the
// metadata (currently the plandrop schema with an image).
type message struct {
	text    string
	preview *preview
}

// preview is the raw card metadata. The thumbnail image is fetched lazily at
// send time from image (a URL), keeping enqueue/coalesce cheap.
type preview struct {
	url         string
	title       string
	description string
	image       string
}

const rlInterval = 30 * time.Second

// number returns the Signal account number, discovering it from the sidecar's
// /v1/accounts on first use so it never has to be configured.
func (r *relay) accountNumber() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.number != "" {
		return r.number, nil
	}
	resp, err := http.Get(r.signalURL + "/v1/accounts")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var accounts []string
	if err := json.NewDecoder(resp.Body).Decode(&accounts); err != nil {
		return "", err
	}
	if len(accounts) == 0 {
		return "", fmt.Errorf("sidecar has no registered accounts")
	}
	r.number = accounts[0]
	return r.number, nil
}

// send delivers to the sidecar with three retries and backoff, then gives up.
// Message bodies are never logged — only source and status.
func (r *relay) send(source string, msg message) {
	number, err := r.accountNumber()
	if err != nil {
		log.Printf("send %s: account discovery: %v", source, err)
		return
	}
	payload := map[string]any{
		"message":    msg.text,
		"number":     number,
		"recipients": []string{number},
	}
	if lp := r.linkPreview(source, msg.preview); lp != nil {
		payload["link_preview"] = lp
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 15 * time.Second}
	for attempt, wait := 0, time.Second; attempt < 3; attempt, wait = attempt+1, wait*3 {
		resp, err := client.Post(r.signalURL+"/v2/send", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 300 {
				log.Printf("sent %s", source)
				return
			}
			err = fmt.Errorf("status %s", resp.Status)
		}
		log.Printf("send %s attempt %d: %v", source, attempt+1, err)
		time.Sleep(wait)
	}
	log.Printf("send %s: giving up", source)
}

// linkPreview builds the sidecar's link_preview object, fetching and base64-
// encoding the thumbnail. Returns nil when there's no preview or the image
// can't be fetched — the send then degrades gracefully to plain text, which
// still carries the URL in its body.
func (r *relay) linkPreview(source string, p *preview) map[string]any {
	if p == nil {
		return nil
	}
	thumb, err := fetchThumbnail(p.image)
	if err != nil {
		log.Printf("send %s: preview image: %v (text-only)", source, err)
		return nil
	}
	return map[string]any{
		"url":              p.url,
		"title":            p.title,
		"description":      p.description,
		"base64_thumbnail": thumb,
	}
}

// fetchThumbnail downloads an image and returns its base64 encoding. Size is
// capped so a misbehaving source can't balloon the send payload.
func fetchThumbnail(url string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("no image url")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty image")
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// enqueue applies per-source rate limiting around send.
func (r *relay) enqueue(source string, msg message) {
	r.rlMu.Lock()
	l, ok := r.rl[source]
	if !ok {
		l = &limiter{}
		r.rl[source] = l
	}
	if since := time.Since(l.last); since >= rlInterval {
		l.last = time.Now()
		r.rlMu.Unlock()
		go r.send(source, msg)
		return
	}
	l.pending = msg
	l.extra++
	if !l.timer {
		l.timer = true
		delay := rlInterval - time.Since(l.last)
		time.AfterFunc(delay, func() { r.flush(source) })
	}
	r.rlMu.Unlock()
}

func (r *relay) flush(source string) {
	r.rlMu.Lock()
	l := r.rl[source]
	msg, extra := l.pending, l.extra
	l.pending, l.extra, l.timer = message{}, 0, false
	l.last = time.Now()
	r.rlMu.Unlock()
	if msg.text == "" {
		return
	}
	if extra > 1 {
		msg.text += fmt.Sprintf(" (+%d more)", extra-1)
	}
	go r.send(source, msg)
}

// format turns an arbitrary JSON payload into a message. Permissive by
// design: recognized shapes get nice formatting, anything else is delivered
// best-effort rather than rejected.
func format(body []byte) message {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return message{text: truncate(strings.TrimSpace(string(body)), 500)}
	}
	str := func(k string) string { s, _ := m[k].(string); return s }

	if msg := str("message"); msg != "" {
		return message{text: msg}
	}
	// plandrop schema: {event, title, url, machine, image, description, ...}
	if ev, title := str("event"), str("title"); ev != "" && title != "" {
		emoji := map[string]string{"created": "📋", "updated": "✏️", "done": "✅"}[ev]
		if emoji == "" {
			emoji = "🔔"
		}
		label := map[string]string{"created": "New plan", "updated": "Plan updated", "done": "Plan done"}[ev]
		if label == "" {
			label = ev
		}
		s := fmt.Sprintf("%s %s: %s", emoji, label, title)
		if mach := str("machine"); mach != "" {
			s += " (" + mach + ")"
		}
		url := str("url")
		if url != "" {
			s += "\n" + url
		}
		if res := str("result"); res != "" && ev == "done" {
			s += "\n→ " + res
		}
		msg := message{text: s}
		// Attach a link preview when the payload carries an image. Signal
		// matches link_preview.url against a URL in the body, which is
		// present above. Older senders omit image/description (omitempty),
		// so this stays backward compatible: no image → plain text.
		if url != "" && str("image") != "" {
			msg.preview = &preview{
				url:         url,
				title:       title,
				description: str("description"),
				image:       str("image"),
			}
		}
		return msg
	}
	// fallback: best-effort extraction of common fields
	for _, k := range []string{"title", "text", "msg"} {
		if v := str(k); v != "" {
			return message{text: v}
		}
	}
	return message{text: truncate(strings.TrimSpace(string(body)), 500)}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (r *relay) hook(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	source := strings.TrimPrefix(req.URL.Path, "/hook/")
	src, ok := r.cfg.Sources[source]
	if !ok {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = req.URL.Query().Get("token")
	}
	if src.Token == "" || token != src.Token {
		log.Printf("hook %s: bad token", source)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !src.Enabled {
		log.Printf("hook %s: disabled, dropped", source)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(req.Body, 64<<10))
	msg := format(body)
	if msg.text == "" {
		msg.text = "(empty notification)"
	}
	if src.Prefix != "" {
		msg.text = src.Prefix + " " + msg.text
	}
	log.Printf("hook %s: accepted", source)
	r.enqueue(source, msg)
	w.WriteHeader(http.StatusAccepted)
}

func (r *relay) healthz(w http.ResponseWriter, _ *http.Request) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(r.signalURL + "/v1/about")
	if err != nil {
		http.Error(w, "sidecar unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		http.Error(w, "sidecar status "+resp.Status, http.StatusBadGateway)
		return
	}
	fmt.Fprintln(w, "ok")
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	r := &relay{
		cfg:       cfg,
		signalURL: envOr("SIGNAL_URL", "http://signal-api:8080"),
		rl:        map[string]*limiter{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/hook/", r.hook)
	mux.HandleFunc("/healthz", r.healthz)
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	addr := envOr("LISTEN", ":8080")
	log.Printf("notify-relay listening on %s, %d sources", addr, len(cfg.Sources))
	log.Fatal(http.ListenAndServe(addr, mux))
}
