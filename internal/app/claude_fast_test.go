package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Captured from api.anthropic.com for an OAuth account whose organization has
// usage credits turned off; identifiers are redacted.
const (
	capturedFastRejectionBody = `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for fast mode."},"request_id":"req_redacted"}`
	capturedExtraUsageOff     = `{"is_enabled":false,"monthly_limit":null,"used_credits":null,"utilization":null,"currency":null,"decimal_places":null,"disabled_reason":null,"user_disabled":true,"spend_limit_reached":false,"credits_can_pay":"no","credits_ever_enabled":true,"daily":null,"weekly":null}`
	fastBody                  = `{"model":"claude-opus-5-5","speed":"fast","messages":[]}`
	standardBody              = `{"model":"claude-opus-5-5","messages":[]}`
)

func writeCapturedFastRejection(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Should-Retry", "false")
	w.Header().Set("Claude-Code-Penguin-Mode-Enabled", "true")
	w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason", "org_level_disabled")
	w.Header().Set("Anthropic-Ratelimit-Unified-Reset", "1796083200")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, capturedFastRejectionBody)
}

func disableExtraUsage(account *Account) *Account {
	enabled := false
	account.adoptExtraUsage(&extraUsagePayload{IsEnabled: &enabled})
	return account
}

func newFastTestServer(t *testing.T, accounts []*Account, respond func(call claudeUpstreamCall, index int, w http.ResponseWriter)) (*server, *claudeFakeUpstream) {
	t.Helper()
	fake, upstream := newClaudeFakeUpstream(t, respond)
	srv := newTestServer(t, accounts)
	srv.upstream = upstream
	srv.lookupAPIKey = func(string) (string, bool, error) { return "test", true, nil }
	return srv, fake
}

func tokensOf(calls []claudeUpstreamCall) []string {
	tokens := make([]string, 0, len(calls))
	for _, call := range calls {
		tokens = append(tokens, call.token)
	}
	return tokens
}

func TestFastRequestsAvoidAccountsWithoutUsageCredits(t *testing.T) {
	a, b := disableExtraUsage(testAccount("account-a", 10)), testAccount("account-b", 10)
	srv, fake := newFastTestServer(t, []*Account{a, b}, func(_ claudeUpstreamCall, _ int, w http.ResponseWriter) { writeClaudeOK(w) })

	for i := range 4 {
		if response := sendClaudeMessage(t, srv, "fast-"+strconv.Itoa(i), fastBody, nil); response.Code != http.StatusOK {
			t.Fatalf("fast status = %d, want 200", response.Code)
		}
	}
	for _, token := range tokensOf(fake.snapshot()) {
		if token != "token-account-b" {
			t.Fatalf("fast request reached %s, want only account-b", token)
		}
	}

	seen := map[string]bool{}
	for i := range 4 {
		sendClaudeMessage(t, srv, "standard-"+strconv.Itoa(i), standardBody, nil)
	}
	for _, token := range tokensOf(fake.snapshot()[4:]) {
		seen[token] = true
	}
	if !seen["token-account-a"] || !seen["token-account-b"] {
		t.Fatalf("standard requests reached %v, want both accounts", seen)
	}
}

func TestFastRejectionFailsOverWithoutTouchingStandardTraffic(t *testing.T) {
	a, b := testAccount("account-a", 10), testAccount("account-b", 10)
	srv, fake := newFastTestServer(t, []*Account{a, b}, func(call claudeUpstreamCall, _ int, w http.ResponseWriter) {
		if call.token == "token-account-a" && strings.Contains(call.body, `"speed"`) {
			writeCapturedFastRejection(w)
			return
		}
		writeClaudeOK(w)
	})

	for i := range 2 {
		response := sendClaudeMessage(t, srv, "session-"+strconv.Itoa(i), fastBody, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 from the account that can serve fast", response.Code)
		}
		if got := response.Header().Get("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"); got != "" {
			t.Fatalf("client saw overage-disabled-reason %q from a rejected attempt", got)
		}
	}
	rejections := 0
	for _, token := range tokensOf(fake.snapshot()) {
		if token == "token-account-a" {
			rejections++
		}
	}
	if rejections != 1 {
		t.Fatalf("account-a received %d fast requests, want 1 before it is blocked", rejections)
	}
	if got := a.modelCooldownUntil("claude-opus-5-5"); !got.IsZero() {
		t.Fatalf("model cooldown = %s, want none for a fast-only rejection", got)
	}
	if a.routingCandidate().cooldown.After(time.Now()) {
		t.Fatal("credential cooldown set for a fast-only rejection")
	}

	before := len(fake.snapshot())
	for i := range 4 {
		sendClaudeMessage(t, srv, "standard-"+strconv.Itoa(i), standardBody, nil)
	}
	usedA := false
	for _, token := range tokensOf(fake.snapshot()[before:]) {
		usedA = usedA || token == "token-account-a"
	}
	if !usedA {
		t.Fatal("standard traffic stopped reaching account-a after its fast rejection")
	}
}

func TestFastRejectionsDoNotSpendTheFailoverBudget(t *testing.T) {
	accounts := []*Account{testAccount("account-a", 10), testAccount("account-b", 10), testAccount("account-c", 10), testAccount("account-d", 10)}
	srv, fake := newFastTestServer(t, accounts, func(call claudeUpstreamCall, _ int, w http.ResponseWriter) {
		if call.token == "token-account-d" {
			writeClaudeOK(w)
			return
		}
		writeCapturedFastRejection(w)
	})

	response := sendClaudeMessage(t, srv, "session-1", fastBody, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d after %v, want the one capable account to serve", response.Code, tokensOf(fake.snapshot()))
	}
	if calls := len(fake.snapshot()); calls > len(accounts) {
		t.Fatalf("upstream calls = %d, want at most one per account", calls)
	}
}

func TestExhaustedFastPoolAnswersWithClientFallback(t *testing.T) {
	a, b := testAccount("account-a", 10), testAccount("account-b", 10)
	srv, fake := newFastTestServer(t, []*Account{a, b}, func(_ claudeUpstreamCall, _ int, w http.ResponseWriter) { writeCapturedFastRejection(w) })

	response := sendClaudeMessage(t, srv, "session-1", fastBody, nil)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if got := response.Header().Get("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"); got != "" {
		t.Fatalf("client saw overage-disabled-reason %q, which makes Claude Code switch fast mode off", got)
	}
	retryAfter, err := strconv.Atoi(response.Header().Get("Retry-After"))
	if err != nil || retryAfter < 20 {
		t.Fatalf("Retry-After = %q, want at least 20 seconds so the client retries at standard speed", response.Header().Get("Retry-After"))
	}
	if calls := len(fake.snapshot()); calls != 2 {
		t.Fatalf("upstream calls = %d, want one per account", calls)
	}

	response = sendClaudeMessage(t, srv, "session-2", fastBody, nil)
	if response.Code != http.StatusTooManyRequests || len(fake.snapshot()) != 2 {
		t.Fatalf("status = %d with %d upstream calls, want 429 without contacting blocked accounts", response.Code, len(fake.snapshot()))
	}
}

func TestFastAndStandardTurnsKeepSeparateOwners(t *testing.T) {
	a, b := testAccount("account-a", 10), testAccount("account-b", 10)
	srv, fake := newFastTestServer(t, []*Account{a, b}, func(_ claudeUpstreamCall, _ int, w http.ResponseWriter) { writeClaudeOK(w) })

	sendClaudeMessage(t, srv, "session-1", standardBody, nil)
	standardOwner, fastOwner := a, "token-account-b"
	if fake.snapshot()[0].token == "token-account-b" {
		standardOwner, fastOwner = b, "token-account-a"
	}
	disableExtraUsage(standardOwner)

	sendClaudeMessage(t, srv, "session-1", fastBody, nil)
	sendClaudeMessage(t, srv, "session-1", standardBody, nil)
	standardOwner.adoptExtraUsage(nil)
	sendClaudeMessage(t, srv, "session-1", `{"model":"claude-opus-5-5(high)","speed":"fast","messages":[]}`, nil)
	got := tokensOf(fake.snapshot())
	want := []string{got[0], fastOwner, got[0], fastOwner}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("turns went to %v, want %v", got, want)
	}
}

func TestUnsupportedSpeedPassesThroughUntouched(t *testing.T) {
	a := testAccount("account-a", 10)
	srv, _ := newFastTestServer(t, []*Account{a}, func(_ claudeUpstreamCall, _ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"'claude-opus-5-5' does not support the `speed` parameter\"}}")
	})
	response := sendClaudeMessage(t, srv, "session-1", fastBody, nil)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "does not support the `speed` parameter") {
		t.Fatalf("response = %d %s, want the upstream 400", response.Code, response.Body.String())
	}
	if !a.fastEligible(time.Now()) {
		t.Fatal("an unsupported-model 400 blocked fast mode for the account")
	}
}

func TestFastRequestHittingSessionLimitKeepsCredentialCooldown(t *testing.T) {
	a, b := testAccount("account-a", 10), testAccount("account-b", 10)
	srv, _ := newFastTestServer(t, []*Account{a, b}, func(call claudeUpstreamCall, _ int, w http.ResponseWriter) {
		if call.token == "token-account-a" {
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeClaudeOK(w)
	})
	for i := range 2 {
		if response := sendClaudeMessage(t, srv, "session-"+strconv.Itoa(i), fastBody, nil); response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 from account-b", response.Code)
		}
	}
	if !a.fastBlockedAt().IsZero() {
		t.Fatal("a session-limit rejection was recorded as a fast-only block")
	}
	if !a.routingCandidate().cooldown.After(time.Now()) {
		t.Fatal("a session-limit rejection did not cool the credential")
	}
}

func TestForceStandardStripsFastFields(t *testing.T) {
	a := testAccount("account-a", 10)
	srv, fake := newFastTestServer(t, []*Account{a}, func(_ claudeUpstreamCall, _ int, w http.ResponseWriter) { writeClaudeOK(w) })
	srv.fastMode.set(fastModeOff)
	disableExtraUsage(a)

	response := sendClaudeMessage(t, srv, "session-1", fastBody, map[string]string{"Anthropic-Beta": "fast-mode-2026-02-01,context-1m-2025-08-07"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 at standard speed", response.Code)
	}
	call := fake.snapshot()[0]
	if strings.Contains(call.body, `"speed"`) {
		t.Fatalf("upstream body %s still requests fast mode", call.body)
	}
	if strings.Contains(call.beta, claudeFastBeta) || !strings.Contains(call.beta, "context-1m-2025-08-07") {
		t.Fatalf("upstream betas = %q, want the fast beta removed and others kept", call.beta)
	}
}

func TestStoredForceFastLoadsAsDefault(t *testing.T) {
	srv := newTestServer(t, nil)
	if err := srv.pool.store.raw.SetFastMode("on"); err != nil {
		t.Fatal(err)
	}
	if err := srv.reloadSettings(); err != nil {
		t.Fatalf("reload with a stored force-fast setting: %v", err)
	}
	if got := srv.fastMode.snapshot(); got != fastModeDefault {
		t.Fatalf("fast mode = %q, want default", got)
	}
}

func TestUsagePollLearnsExtraUsage(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/usage" {
			_, _ = io.WriteString(w, `{"five_hour":{"utilization":10,"resets_at":null},"extra_usage":`+capturedExtraUsageOff+`}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)
	previousAccount, previousProfile := accountAPIBaseURL, claudeOAuthProfileURL
	accountAPIBaseURL = api.URL + "/api/oauth"
	claudeOAuthProfileURL = api.URL + "/api/oauth/profile"
	t.Cleanup(func() { accountAPIBaseURL, claudeOAuthProfileURL = previousAccount, previousProfile })
	account := testAccount("account-a", 10)
	srv := newTestServer(t, []*Account{account})

	if !account.fastEligible(time.Now()) {
		t.Fatal("an account with unknown extra usage should be tried for fast mode")
	}
	if err := srv.pollUsage(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	if account.fastEligible(time.Now()) {
		t.Fatal("an account with usage credits off is still eligible for fast mode")
	}
}

func TestModelQuotaRejectionOnFastRequestCoolsOnlyTheModel(t *testing.T) {
	a, b := testAccount("account-a", 10), testAccount("account-b", 10)
	srv, _ := newFastTestServer(t, []*Account{a, b}, func(call claudeUpstreamCall, _ int, w http.ResponseWriter) {
		if call.token == "token-account-a" {
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
			w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
			w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeClaudeOK(w)
	})
	for i := range 2 {
		if response := sendClaudeMessage(t, srv, "session-"+strconv.Itoa(i), fastBody, nil); response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 from account-b", response.Code)
		}
	}
	if !a.fastBlockedAt().IsZero() {
		t.Fatal("a model quota rejection blocked fast mode for every model on the account")
	}
	if !a.modelCooldownUntil("claude-opus-5-5").After(time.Now()) {
		t.Fatal("a model quota rejection did not cool the model")
	}
}

func TestUnknownSpeedIsForwardedForUpstreamValidation(t *testing.T) {
	a := disableExtraUsage(testAccount("account-a", 10))
	srv, fake := newFastTestServer(t, []*Account{a}, func(_ claudeUpstreamCall, _ int, w http.ResponseWriter) { writeClaudeOK(w) })
	response := sendClaudeMessage(t, srv, "session-1", `{"model":"claude-opus-5-5","speed":"turbo","messages":[]}`, nil)
	if response.Code != http.StatusOK || len(fake.snapshot()) != 1 {
		t.Fatalf("status = %d with %d upstream calls, want the request forwarded", response.Code, len(fake.snapshot()))
	}
}
