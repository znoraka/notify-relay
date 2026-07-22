# notify-relay

Webhook-to-Signal relay. Projects POST JSON to `https://notify.gawaak.ovh/hook/<source>`
with a per-source bearer token; the relay formats the payload and delivers it to the
`signal-api` sidecar (signal-cli-rest-api) as Note-to-Self.

## Integrate a project (two lines)

```sh
curl -X POST https://notify.gawaak.ovh/hook/mysource \
  -H "Authorization: Bearer $TOKEN" -d '{"message":"backup finished ✔"}'
```

The token may also be passed as `?token=...` for callers that can't set headers
(e.g. plandrop's per-user webhook URL).

## Payloads (permissive)

- `{"message": "..."}` — sent as-is (with the source's prefix, if any)
- plandrop schema `{event, title, url, machine, result, image, description}` —
  formatted with 📋 created / ✏️ updated / ✅ done. When `image` is present the
  relay downloads it, base64-encodes it, and attaches a Signal link preview
  (card with title + description + thumbnail) to the message — Signal fetches no
  previews recipient-side, so the sender must supply them. If the image can't be
  fetched, delivery falls back to plain text. `image`/`description` are
  optional, so older senders keep working unchanged.
- any other JSON — best-effort extraction of `title`/`text`/`msg`
- non-JSON — delivered raw (truncated at 500 chars)

Requests are never rejected for shape — malformed payloads still attempt delivery.

## Config

`CONFIG` (default `/etc/notify-relay/sources.json`), or `SOURCES_JSON` env as fallback:

```json
{
  "sources": {
    "plandrop": {"token": "long-random-string", "prefix": "", "enabled": true},
    "adhoc":    {"token": "another-long-string", "prefix": "[adhoc]", "enabled": true}
  }
}
```

Token rotation = edit file, redeploy. Env: `SIGNAL_URL` (default
`http://signal-api:8080`), `LISTEN` (default `:8080`). The Signal account number
is auto-discovered from the sidecar's `/v1/accounts`.

## Behavior

- 3 delivery retries with backoff, then log-and-discard. No queue, no persistence.
- Per-source rate limit: 1 message / 30 s; excess coalesces into a trailing
  send with `(+N more)`.
- Message bodies are never logged at info level — only source + status.
- `GET /healthz` pings the sidecar.
