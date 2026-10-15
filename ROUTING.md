# Routing and account safety

`claude-balancer` treats an account switch as a retry of the same request, not
as a second independent generation. The router therefore separates selecting
an account, accepting a request, and committing streamed output. A route can
move while those first two stages are still reversible; it must stay on the
same account after account-bound work has been sent.

## Account selection

Each account has a routing mode (`normal`, `priority`, or `paused`), a
credential state, model-level cooldowns, and the latest five-hour and seven-day
usage windows. Selection removes paused, reauthentication-required, and
cooldown accounts before comparing priority and available capacity. Included
quota is preferred. A positive credit balance can be used only when no eligible
account has included quota and spend limits allow it.

The established owner of a conversation outranks account priority. The affinity
key combines provider, model, session ID, and agent ID. The router reads these
values from `X-Claude-Code-Session-Id`, `X-Claude-Code-Agent-Id`, and the request
metadata `user_id.session_id` and `parent_session_id`. A request without an
established owner can use the normal weighted selection; a request with an
owner remains there while that owner is healthy.

A route claim is provisional until the upstream accepts the request. The local
claim is released when setup fails before any upstream write. Durable ownership
and retry barriers are written to SQLite after acceptance so another process
sharing the database can preserve the same owner.

## When an account switch is safe

A switch is safe only when all of the following hold:

1. The previous account has not accepted the request or emitted a response.
2. The full message history is available to replay and does not contain
   account-bound encrypted state that the replacement cannot read.
3. The failure is retryable and the request has not already produced downstream
   output.

The router may retry setup failures, credential refresh failures, and a
pre-acceptance rate limit on another eligible account. It does not resend a
request after streamed output, `message_start`, or an accepted response has
reached the client. It does not move a healthy established conversation merely
because another account has a higher priority.

If the owner is unavailable before acceptance and the request is portable, the
router makes one bounded retry on another account. Once the replacement emits
`message_start`, ownership is persisted to that account. If no safe replacement
exists, the original upstream error is returned with a local retry hint.

## Limits and cooldowns

The upstream error determines the scope of a cooldown:

- A model-level `429` cools down that model on the account.
- Claude's five-hour or seven-day usage limit cools down the credential across
  models until the advertised reset time.
- A temporary upstream transport failure is request-scoped and does not poison
  an account.
- Invalid input, permission errors, and other request-scoped failures are
  returned as-is. They do not rotate accounts or poison a pool. A model that is
  unavailable on one account can be cooled down for that account when the
  upstream identifies it as an account capability failure.

`401` starts a credential refresh. A successful refresh retries the setup once.
An `invalid_grant` or otherwise terminal refresh result marks the account for
reauthentication and removes it from selection until the `/accounts` flow
completes.

`Retry-After` is treated as a local hint and is clamped to a safe minimum before
it is exposed to clients. The router never sleeps a request for an unbounded
period and never turns a client cancellation into an account penalty.

## Streaming and reconnects

The Messages API stream is committed when the upstream sends `message_start` or
another response event that proves the request was accepted. Before that point,
a dropped connection can be retried if the request is portable. After that point,
the balancer closes the downstream stream with a typed transport error; the
client owns reconnect and replay policy.

A reconnect carrying the same session and agent affinity keeps the established
owner. A request carrying an account-bound turn token or encrypted content is
never replayed to an account that did not issue it. A reconnect with complete
portable messages may use the bounded replacement path when the original owner
is unavailable before acceptance.

## HTTP behavior

`POST /v1/messages` is the canonical route. `POST /messages` is an alias for
clients whose configured base URL already includes `/v1`. The balancer supports
streaming and non-streaming Messages API requests and rejects unsupported
methods before account selection. It forwards the selected account's
`anthropic-version` and approved Claude headers while removing client
credentials, cookies, and connection-specific headers.

A successful upstream response does not imply that the client consumed all of
the output. Logs distinguish upstream acceptance, downstream writes, and final
completion. Usage is recorded only when the upstream provides it; missing or
partial usage is not invented.

## Administration

Changing an account to `paused` retires its active streams and prevents new
claims. `normal` and `priority` changes apply to the next selection without
rewriting an established owner. Removing an account retires its streams and
removes it from future claims. These operations are persisted in SQLite and are
picked up by other server processes on their settings poll.

The dashboard exposes routing reasons, prior and selected owners, cooldown
state, and accepted switches without exposing credentials. Use the admin page
for full identifiers and event metadata when diagnosing a route.
