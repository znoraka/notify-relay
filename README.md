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

## T3 Code relay (optional)

The same binary also serves a minimal [T3 Code](https://github.com/pingdotgg/t3code)
relay, so the phone app's **Device Notifications** switch works against
self-hosted infrastructure instead of the hosted Cloudflare relay. It is
entirely separate from the Signal half above and shares nothing but the port.

Enabled only when all of these are set — otherwise the T3 routes are skipped and
the service stays a plain webhook-to-Signal relay:

```sh
APNS_TEAM_ID=...          # Apple developer team
APNS_KEY_ID=...           # Key ID of the .p8
APNS_PRIVATE_KEY=...      # .p8 contents; literal \n allowed for one-line env vars
APNS_BUNDLE_ID=...        # default APNs topic, e.g. dev.ezag.t3code.preview
APNS_ENVIRONMENT=production   # or sandbox for development-signed builds
T3_ENV_CREDENTIAL=...     # shared secret the environment presents when publishing
T3_STATE=/var/lib/notify-relay/t3-devices.json   # needs a persistent volume
```

Routes: `/health`, `/.well-known/oauth-*`, `/v1/client/dpop-token`,
`/v1/client/devices`, `/v1/environments`, `/v1/mobile/*`, and
`/v1/environments/{env}/threads/{thread}/agent-activity`.

Point an environment at it by writing the credentials into its secret store:

```sh
# The environment token comes from the environment itself; `t3 connect` needs
# Clerk and is unavailable on a Clerk-less build.
TOKEN=$(t3 auth session issue --token-only | grep . | tail -1)

# cloudMintPublicKey is validated as a real Ed25519 key even though this relay
# never mints credentials, so generate a throwaway pair once:
#   openssl genpkey -algorithm ed25519 -out mint.key
#   openssl pkey -in mint.key -pubout -out mint.pub
MINT=$(awk 'BEGIN{ORS="\\n"} {print}' mint.pub)

curl -X POST http://127.0.0.1:3773/api/connect/relay-config \
  -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -d '{"relayUrl":"https://notify.gawaak.ovh","cloudUserId":"local",
       "environmentCredential":"'"$T3_ENV_CREDENTIAL"'","cloudMintPublicKey":"'"$MINT"'",
       "endpointRuntime":null}'

# Publishing is off until switched on:
curl -X POST http://127.0.0.1:3773/api/connect/preferences \
  -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -d '{"publishAgentActivity":true}'
```

### Security model

Single user, so the hosted relay's defences are deliberately absent: DPoP proofs
are accepted without verification, the environment's signed publish proof is
ignored, and there is no Clerk. Auth is `T3_ENV_CREDENTIAL` for publishing plus
any non-empty bearer for the app. Holding either lets someone push a
notification to one phone — that is the entire blast radius.

### Not implemented

Live Activities register successfully but no updates are ever pushed, so leave
**Live Activity Updates** off in the app until that lands. `/v1/environments`
always returns an empty list: environments are linked by writing to their secret
store directly, so the relay never learns about them.
