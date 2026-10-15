# claude-balancer

![Claude Balancer dashboard](https://github.com/user-attachments/assets/530effd5-a001-485b-bc92-23340aadfd37)

`claude-balancer` shares Claude usage across several authenticated accounts. It keeps account credentials in SQLite, preserves conversation affinity, and only moves a request when the current account cannot safely accept it.

## Install

```sh
go install github.com/supabitapp/claude-balancer@latest
```

## Run the proxy

```sh
claude-balancer server
```

The server listens on `http://127.0.0.1:8317` by default.

- `/v1/messages` and `/messages` accept Anthropic Messages API `POST` requests. Streaming and non-streaming responses are supported.
- `/v1/models` lists the models available in the account pool.
- `/dashboard` shows redacted account health and traffic.
- `/stats` returns JSON account and token statistics.
- `/accounts` starts the account sign-in flow. It is safe to share this route with someone who should add an account without seeing existing credentials.
- `/admin` is the authenticated administration dashboard.

The terminal UI and admin dashboard can set each account to `normal`, `priority`, or `paused`. A paused account receives no new requests and its existing connections are retired so that clients can reconnect elsewhere.

## Manage accounts and keys

```sh
claude-balancer accounts add
claude-balancer accounts list
claude-balancer accounts mode you@example.com priority
claude-balancer accounts mode you@example.com paused
claude-balancer accounts mode you@example.com normal
```

Create client keys on the server and revoke them when no longer needed:

```sh
claude-balancer keys add my-laptop
claude-balancer keys list
claude-balancer keys rm my-laptop
```

The database is `~/.claude-balancer/state.db` unless `-state` selects another location. Protect the database file and its backups because they contain account tokens and client key material.

## Configure a Claude client

Create a key, then point any Anthropic Messages API client at the balancer:

```sh
export CLAUDE_BALANCER_API_KEY="<server-key>"
export ANTHROPIC_BASE_URL="http://127.0.0.1:8317"
export ANTHROPIC_API_KEY="$CLAUDE_BALANCER_API_KEY"
```

Requests must include the normal `anthropic-version` header. The balancer forwards the supported Claude headers and strips credentials and connection headers before sending the request to the selected account.

For clients that use an explicit endpoint, set the base URL to `http://127.0.0.1:8317` and send `POST /v1/messages`. The `/messages` alias is provided for clients that already append `/v1` to their configured base URL.

## Routing and safe account changes

Conversation-affinity headers (`X-Claude-Code-Session-Id` and `X-Claude-Code-Agent-Id`) and request metadata session identifiers keep a conversation on its established account. An established owner outranks account priority. The balancer can move a request only before a response is accepted, when the complete message history is portable, or when the upstream connection has not transmitted work. Once streaming output or account-bound state exists, the owner is retained and the client receives the upstream failure.

Usage limits are handled at the account and model level. A model throttle cools down that model on the account. Claude's five-hour and seven-day limits cool down the credential across models. Authentication failures trigger a refresh; an invalid refresh token marks the account for sign-in. Invalid input, permission, and other request-scoped errors are returned to the client without rotating accounts.

## Observability

Use JSON logs for request-correlated diagnostics:

```sh
claude-balancer server -no-tui -json
```

Responses include an `X-Claude-Balancer-Request-Id` header. OpenTelemetry is optional:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 \
  claude-balancer server -no-tui -json -otel
```

See [OBSERVABILITY.md](OBSERVABILITY.md) for trace fields and account-switch diagnostics. [ROUTING.md](ROUTING.md) describes the admission, retry, cooldown, and conversation-affinity rules in detail.
