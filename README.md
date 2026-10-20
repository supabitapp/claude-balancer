# claude-balancer

<img width="2428" height="1122" alt="CleanShot 2026-10-15 at 23 49 01@2x" src="https://github.com/user-attachments/assets/34f61b6f-9512-41bf-9500-a355cab63065" />


`claude-balancer` is a Go proxy that shares Claude Messages API traffic across
multiple authenticated accounts. It stores account state, route ownership, API
keys, and usage history in SQLite. The dashboard uses server-rendered HTML with
HTMX and keeps the existing visual style.

The proxy preserves conversation affinity and only retries a request while the
upstream work is still safe to replay. It supports streaming and non-streaming
Messages API requests, count-tokens requests, model discovery, and
speech-to-text dictation.

## Install

```sh
go install github.com/supabitapp/claude-balancer@latest
```

The default state database is `~/.claude-balancer/state.db`. Use `-state` on
the relevant command when the database lives elsewhere.

## First run

Provision a client key. The command prints the secret once, so store it in a
password manager or another protected secret store:

```sh
claude-balancer keys add my-laptop
```

Add a Claude account from the machine running the CLI:

```sh
claude-balancer accounts add
```

The command opens Claude sign-in and listens for the OAuth callback on
`127.0.0.1:54545`. For a server without a local browser, start the server,
open `/accounts`, complete sign-in, then paste the complete localhost callback
address into the form. The authorization code is exchanged by the balancer and
the account is added to the pool.

Start the proxy:

```sh
claude-balancer server
```

The default listener is `http://127.0.0.1:8317`. A server refuses to start
without an active client key unless you explicitly pass `-no-auth` for an
isolated local process.

## Configure a client

Point an Anthropic Messages API client at the proxy and provide the provisioned
key. Both `Authorization: Bearer ...` and `x-api-key` are accepted.

```sh
export ANTHROPIC_BASE_URL="http://127.0.0.1:8317"
export ANTHROPIC_API_KEY="<server-key>"
```

Use `POST /v1/messages` for clients that expect the standard Messages API base
URL. The `/messages` alias is available for clients whose configured base URL
already includes `/v1`.

## HTTP routes

| Route | Purpose | Authentication |
| --- | --- | --- |
| `/dashboard` | Redacted account health, limits, traffic, and active threads | None |
| `/accounts` | Start browser-based account sign-in | None |
| `/admin` | Full account controls, API key management, and event details | Admin password |
| `/v1/messages` | Claude Messages API | Client API key |
| `/messages` | Messages API alias | Client API key |
| `/v1/messages/count_tokens` | Claude count-tokens API | Client API key |
| `/v1/models` | Models available through the account pool | Client API key |
| `/api/ws/speech_to_text/voice_stream` | Claude speech-to-text WebSocket relay | Client API key |
| `/stats` | JSON account and token statistics | None |

The root path redirects to `/dashboard`. Set or reset the admin password with:

```sh
claude-balancer admin password
```

See [ADMIN.md](ADMIN.md) for password storage, session security, and dashboard
controls.

## Manage accounts and keys

```sh
claude-balancer accounts list
claude-balancer accounts mode you@example.com priority
claude-balancer accounts mode you@example.com normal
claude-balancer accounts mode you@example.com paused
claude-balancer accounts rm you@example.com

claude-balancer keys list
claude-balancer keys rm my-laptop
```

`normal` accounts participate in the standard round-robin selector. `priority`
accounts are selected before normal accounts. `paused` accounts receive no new
requests and their active connections are retired so clients can reconnect
elsewhere.

## Routing behavior

The router uses these signals to keep a conversation on its selected account:

- `X-Claude-Code-Session-Id` identifies the root conversation.
- `X-Claude-Code-Agent-Id` gives a subagent its own route while inheriting the
  parent account when possible.
- `X-Claude-Code-Parent-Agent-Id` preserves nested subagent lineage.
- Claude request metadata supplies session and parent identifiers when headers
  are absent.

Route ownership is scoped by the canonical model, so different models can use
different eligible accounts for the same conversation. Thinking suffixes such
as `(high)` and numeric thinking budgets share the base model route. Route
claims are reserved before upstream execution with a SQLite compare-and-set
write. A delayed result from an older account cannot overwrite a newer
failover, and a child route cannot rewrite its parent's route.

The selector removes paused, reauthentication-required, cooling, and exhausted
accounts before applying priority and round-robin selection. Account usage is
shown separately for the five-hour window, the Fable-scoped seven-day window,
and the general seven-day window.

Retries are limited to failures that are safe to replay:

- A `401` refreshes the selected account once before another account is tried.
- A credential-wide five-hour or seven-day rejection cools the account across
  models until its reset window.
- A Fable or overage-only rejection cools only the affected model. Other models
  on the same account remain eligible.
- Fast entitlement errors are returned without rotating or cooling the account.
- A streaming request can move only before the first complete SSE data event.
  Once output is accepted, later stream errors are passed through without
  replaying the request.

Read [ROUTING.md](ROUTING.md) for the complete admission, cooldown, retry, and
conversation-affinity rules.

## Speech to text

`GET /api/ws/speech_to_text/voice_stream` relays Claude's dictation WebSocket
through a pool account. It uses the same path as Anthropic, so a client changes
only the host and sends the balancer key instead of a claude.ai token:

```text
wss://<balancer>/api/ws/speech_to_text/voice_stream?encoding=linear16&sample_rate=16000&channels=1&endpointing_ms=300&utterance_end_ms=1000&language=en&use_conversation_engine=true
Authorization: Bearer <server-key>
x-config-keyterms: Supaterm,Tailscale
```

The client sends raw PCM16 little-endian 16 kHz mono audio as binary frames,
plus `{"type":"KeepAlive"}` and `{"type":"CloseStream"}` text frames. The server
returns `TranscriptText` frames with the cumulative transcript, followed by
`TranscriptEndpoint`. Frames are relayed unchanged in both directions. Keep each
frame at or below 1 MiB. A session ends after six minutes, or after 30 seconds
without a frame in either direction.

## Observability

Run with structured logs:

```sh
claude-balancer server -no-tui -json
```

Responses include an `X-Claude-Balancer-Request-Id` header. OpenTelemetry is
optional:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 \
  claude-balancer server -no-tui -json -otel
```

See [OBSERVABILITY.md](OBSERVABILITY.md) for trace fields, request events, and
account-switch diagnostics.

## Storage and deployment

The SQLite database contains account tokens, API key secrets, route ownership,
admin sessions, and usage history. Keep the file and its backups private. The
server should run behind HTTPS when it is reachable beyond loopback. Do not
commit API keys, OAuth tokens, SSH keys, deployment hosts, or database backups.

The repository includes a systemd user unit and
`deploy/deploy-claude-balancer`. The deployment script builds the revision on
the target machine, installs the binary, restarts the service when needed, and
verifies `/stats` before reporting success. Configure deployment credentials in
the deployment system's secret store rather than in the repository.

## Development

Run the complete local verification suite:

```sh
./scripts/verify
```

This runs `go vet`, the race-enabled test suite, and a trimmed production build.
