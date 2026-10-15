# HTTP Messages logs and traces

`POST /v1/messages` and its `/messages` alias emit correlated structured logs
without requiring a collector. Optional OpenTelemetry tracing exports the same
request lifecycle over OTLP HTTP. Observability does not select accounts, replay
inference, or change account ownership.

## Enable logs and tracing

For JSON logs on stderr and the rotating log file:

```sh
claude-balancer server -no-tui -json
```

HTTP records use `msg="messages request"`, a `stage`, and an independently
generated `request_id`. The response header `X-Claude-Balancer-Request-Id`
exposes that ID for a client bug report. INFO covers start, acceptance, usage,
terminal, and final summaries. DEBUG includes candidate selection, setup
attempts, and each valid upstream or SSE event's framing metadata; payloads and
prompt text are never logged.

Enable actual traces with `-otel` and an OTLP HTTP collector:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:4318"
export OTEL_SERVICE_NAME="claude-balancer"
claude-balancer server -no-tui -json -otel
```

`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` can instead specify the full traces URL.
The exporter supports the standard OTLP HTTP header, TLS/certificate,
compression, and protocol environment settings. Keep collector credentials in
`OTEL_EXPORTER_OTLP_TRACES_HEADERS`, not the endpoint URL. Tracing is off by
default even when OTLP variables are present.

Sampling uses the OTel SDK's `OTEL_TRACES_SAMPLER` and
`OTEL_TRACES_SAMPLER_ARG`. Unsampled requests still have trace and span IDs in
logs; logging-only mode always has a `request_id`.

## Request lifecycle

A normal request has spans similar to:

```text
POST /v1/messages
  http.request.read
  claude.request.translate
  claude.response
```

Credential refresh and pool recovery create independent spans that can be linked
to waiting requests. Refresh completion includes persistence, publication, and
invalidation; it does not borrow the first request's cancellation lifetime.

Useful request stages are:

| Stage | What it establishes |
| --- | --- |
| `admission`, `authorization`, `body_read` | Authentication, content encoding, wire and decoded sizes, and read latency. |
| `method_not_allowed`, `rejected` | Unsupported methods or requests rejected before upstream setup. |
| `normalized` | Model, input/tool counts, stream mode, and portable-history metadata. |
| `route_selected`, `routing_candidate` | Selected and prior owners, routing reason, provisional claim, and quota state. |
| `handshake_started`, `handshake_finished` | Upstream setup latency and status before acceptance. |
| `account_switch_ready`, `reconnect_decision` | Whether a replacement account can safely receive the complete request. |
| `http_response_headers` | Upstream HTTP status after a non-streaming request. |
| `upstream_write_started`, `upstream_write_finished` | The transmission boundary; a failed write may have partially transmitted data. |
| `response_accepted` | Acceptance at `message_start`, persisted ownership, and any deduplicated switch. |
| `first_upstream_event`, `first_delta` | First-event and first-token latency. |
| `upstream_event`, `downstream_event` | Event type, size, index, and sequence metadata without payloads. |
| `upstream_rejected`, `failure` | Upstream rejection or transport failure; the client owns replay. |
| `usage` | Actual input, cache-read, cache-write, output, and reasoning token counts. |
| `terminal`, `cleanup`, `finished` | Terminal kind, cleanup, admission release, counts, elapsed time, and cancellation state. |

`http_status` is the status chosen by the adapter. A successful write or flush
is not proof the client consumed the output. Missing or incomplete usage is not
invented. Cancellation can prevent delivery while still producing a final
trace outcome.

## Diagnose an account switch

For a client-reported request ID:

```sh
jq 'select(.msg == "messages request" and .request_id == "REQUEST_ID")' server.log
```

Read the events in this order:

1. `route_selected` identifies the established owner, if any.
2. `account_switch_ready` records whether the complete request is portable.
3. `response_accepted` marks the point after which replay is unsafe.
4. `failure` and `terminal` show whether the client saw a transport or usage
   error.

An account move before `response_accepted` can be a safe bounded retry. A move
after acceptance indicates a routing bug and should be investigated. Account
IDs, session IDs, and agent IDs are hashed in the public dashboard; use `/admin`
for full values.

## Log inspection

`scripts/check-logs` reads only selected metadata over SSH and never prints raw
request or response bodies:

```sh
scripts/check-logs --host balancer --service claude-balancer.service
scripts/check-logs --errors --since "15 minutes ago"
```
