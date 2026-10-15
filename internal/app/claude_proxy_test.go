package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaudeBodyForAccountEncodesUserIDAsString(t *testing.T) {
	account := accountFromState(accountState{
		IDToken: claudeIdentityToken(claudeCredentials{
			AccountID: "account-uuid",
			DeviceID:  "device-id",
		}),
	})
	body := []byte(`{"model":"claude-sonnet","metadata":{"user_id":{"session_id":"session-id"}}}`)
	got := claudeBodyForAccount(body, account, claudeRoute{thread: "thread-id", session: "session-id"})

	var payload struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Metadata.UserID == "" {
		t.Fatal("metadata.user_id is empty")
	}
	var identity map[string]string
	if err := json.Unmarshal([]byte(payload.Metadata.UserID), &identity); err != nil {
		t.Fatalf("metadata.user_id = %q is not an encoded JSON string: %v", payload.Metadata.UserID, err)
	}
	for key, want := range map[string]string{
		"account_uuid":      "account-uuid",
		"device_id":         "device-id",
		"session_id":        "session-id",
		"parent_session_id": "thread-id",
	} {
		if identity[key] != want {
			t.Fatalf("metadata.user_id[%q] = %q, want %q", key, identity[key], want)
		}
	}
}

func TestClaudeModelsSetsOAuthHeaders(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	t.Cleanup(upstream.Close)

	srv := newTestServer(t, []*Account{accountFromState(accountState{
		IDToken:     claudeIdentityToken(claudeCredentials{AccountID: "account-uuid"}),
		AccessToken: "access-token",
	})})
	srv.upstream = upstream.URL
	srv.lookupAPIKey = func(string) (string, bool, error) { return "test", true, nil }

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer test")
	response := httptest.NewRecorder()
	srv.claudeModels(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	for name, want := range map[string]string{
		"Authorization":     "Bearer access-token",
		"Accept":            "application/json",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"X-App": "cli",
	} {
		if got := seen.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	for _, beta := range []string{"oauth-2025-04-20", "claude-code-20250219"} {
		if !strings.Contains(seen.Get("Anthropic-Beta"), beta) {
			t.Fatalf("Anthropic-Beta = %q, want %q", seen.Get("Anthropic-Beta"), beta)
		}
	}
}

func TestClaudeCountTokensForwardsBodyWithoutMetadata(t *testing.T) {
	var forwarded []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		forwarded, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read forwarded body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":7}`)
	}))
	t.Cleanup(upstream.Close)

	account := accountFromState(accountState{
		IDToken:     claudeIdentityToken(claudeCredentials{AccountID: "account-uuid", DeviceID: "device-id"}),
		AccessToken: "access-token",
	})
	srv := newTestServer(t, []*Account{account})
	srv.upstream = upstream.URL
	body := []byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hello"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	response, err := srv.doClaudeMessages(context.Background(), request, body, account)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if string(forwarded) != string(body) {
		t.Fatalf("forwarded body = %s, want %s", forwarded, body)
	}
}

func TestClaudeMessagesRecordStats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_test","model":"claude-haiku-5-5","usage":{"input_tokens":11,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}}`)
	}))
	t.Cleanup(upstream.Close)

	account := accountFromState(accountState{
		IDToken:     claudeIdentityToken(claudeCredentials{AccountID: "account-uuid", DeviceID: "device-id"}),
		AccessToken: "access-token",
	})
	srv := newTestServer(t, []*Account{account})
	srv.upstream = upstream.URL
	srv.lookupAPIKey = func(string) (string, bool, error) { return "test", true, nil }

	body := `{"model":"claude-haiku-5-5","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly OK."}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test")
	response := httptest.NewRecorder()
	srv.claudeMessagesHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	snapshot := srv.stats.snapshot()
	if snapshot.Turns != 1 {
		t.Fatalf("turns = %d, want 1", snapshot.Turns)
	}
	traffic, ok := snapshot.Accounts[account.id()]
	if !ok {
		t.Fatalf("missing stats for account %q", account.id())
	}
	if traffic.Turns != 1 || activityTotal(traffic.Activity) != 1 {
		t.Fatalf("account traffic = turns %d activity %d, want 1 and 1", traffic.Turns, activityTotal(traffic.Activity))
	}
	if snapshot.MonthlyUsage.InputTokens != 16 || snapshot.MonthlyUsage.OutputTokens != 4 || snapshot.MonthlyUsage.InputDetails.CachedTokens != 3 || snapshot.MonthlyUsage.InputDetails.CacheWriteTokens != 2 {
		t.Fatalf("monthly usage = %+v, want input 16 cached 3 cache-write 2 output 4", snapshot.MonthlyUsage)
	}
}

func TestClaudeMessagesShowLiveThreadWhileStreaming(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11,\"cache_read_input_tokens\":3,\"output_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":11,\"cache_read_input_tokens\":3,\"output_tokens\":4}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	var releaseOnce sync.Once
	releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseStream)

	account := testAccount("account-a", 10)
	srv := newTestServer(t, []*Account{account})
	srv.upstream = upstream.URL
	if err := srv.pool.store.addAPIKey(storedAPIKey{Name: "laptop", Secret: "laptop-secret", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv.lookupAPIKey = srv.pool.store.apiKeyName

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer laptop-secret")
	request.Header.Set("X-Claude-Code-Session-Id", "session-a")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.claudeMessagesHTTP(response, request)
	}()

	var threads []ThreadSnapshot
	for deadline := time.Now().Add(5 * time.Second); len(threads) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		threads = srv.stats.snapshot().Threads
	}
	if len(threads) != 1 {
		t.Fatalf("live threads = %d, want 1 while streaming", len(threads))
	}
	thread := threads[0]
	if thread.Key != "claude:claude-sonnet:session-a" || thread.Account != account.id() || thread.apiKeyName != "laptop" || thread.Via != transportHTTP || thread.Turns != 1 {
		t.Fatalf("live thread = %+v, want session route on account-a from laptop", thread)
	}

	releaseStream()
	<-done
	snapshot := srv.stats.snapshot()
	if len(snapshot.Threads) != 0 {
		t.Fatalf("live threads = %d, want 0 after the stream ends", len(snapshot.Threads))
	}
	if snapshot.MonthlyUsage.InputTokens != 14 || snapshot.MonthlyUsage.InputDetails.CachedTokens != 3 || snapshot.MonthlyUsage.OutputTokens != 4 {
		t.Fatalf("monthly usage = %+v, want input 14 cached 3 output 4", snapshot.MonthlyUsage)
	}
	usage, err := srv.pool.store.apiKeyUsage()
	if err != nil || usage["laptop"].TotalTokens != 18 {
		t.Fatalf("API key usage = %+v, error = %v, want 18 tokens for laptop", usage, err)
	}
}

func TestClaudeResponseUsageParsesStream(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11,\"cache_read_input_tokens\":3}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n"
	usage := claudeResponseUsage([]byte(body))
	if usage.InputTokens != 14 || usage.OutputTokens != 4 || usage.InputDetails.CachedTokens != 3 || usage.TotalTokens != 18 {
		t.Fatalf("stream usage = %+v, want input 14 cached 3 output 4 total 18", usage)
	}
}

func TestClaudeMessagesReportsModelCooldown(t *testing.T) {
	account := testAccount("account-uuid", 10)
	account.coolModel("claude-sonnet", nil)
	srv := newTestServer(t, []*Account{account})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","max_tokens":1,"messages":[{"role":"user","content":"hello"}]}`))
	response := httptest.NewRecorder()
	srv.claudeMessagesHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusTooManyRequests, response.Body.String())
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header is empty")
	}
	if got := srv.claudeModelRetryAfter("claude-sonnet"); got <= 0 || got > time.Minute {
		t.Fatalf("retry duration = %s, want positive duration under one minute", got)
	}
}

func TestClaudeMessagesSkipsExhaustedFableWindow(t *testing.T) {
	account := testAccount("account-uuid", 10)
	account.fable = window{usedPercent: 100, minutes: 7 * 24 * 60, resetsAt: time.Now().Add(time.Hour), seenAt: time.Now()}
	srv := newTestServer(t, []*Account{account})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-fable-5-1","max_tokens":1,"messages":[{"role":"user","content":"hello"}]}`))
	response := httptest.NewRecorder()
	srv.claudeMessagesHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusTooManyRequests, response.Body.String())
	}
}

func TestClaudeMessagesSkipsExhaustedFiveHourWindow(t *testing.T) {
	account := testAccount("account-uuid", 10)
	account.primary = window{usedPercent: 100, minutes: 5 * 60, resetsAt: time.Now().Add(time.Hour), seenAt: time.Now()}
	srv := newTestServer(t, []*Account{account})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hello"}]}`))
	response := httptest.NewRecorder()
	srv.claudeMessagesHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusTooManyRequests, response.Body.String())
	}
}

func TestClaudeRouteFromPreservesSubagentHierarchy(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header.Set("X-Claude-Code-Session-Id", "root-session")
	request.Header.Set("X-Claude-Code-Agent-Id", "child-agent")
	request.Header.Set("X-Claude-Code-Parent-Agent-Id", "parent-agent")
	route := claudeRouteFrom(request, claudeMessageRequest{})
	keys := route.keys("claude-sonnet-4-5(high)")
	if len(keys) != 3 {
		t.Fatalf("route keys = %v, want child, parent, root", keys)
	}
	if keys[0] != "claude:claude-sonnet-4-5:root-session:agent:child-agent" || keys[1] != "claude:claude-sonnet-4-5:root-session:agent:parent-agent" || keys[2] != "claude:claude-sonnet-4-5:root-session" {
		t.Fatalf("route keys = %v, want canonical model and agent hierarchy", keys)
	}
}

func TestClaudeRouteFromParsesLegacyMetadataSession(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := claudeMessageRequest{}
	body.Metadata.UserID = json.RawMessage(`"user_account_session_1234"`)
	route := claudeRouteFrom(request, body)
	if route.session != "1234" {
		t.Fatalf("session = %q, want legacy session suffix", route.session)
	}
}

func TestClaude429ScopeDistinguishesCredentialModelAndRequest(t *testing.T) {
	credential := http.Header{"anthropic-ratelimit-unified-5h-status": {"rejected"}}
	if got := claude429ScopeFor(credential, nil); got != claude429Credential {
		t.Fatalf("credential scope = %v, want credential", got)
	}
	model := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed_warning"},
		"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
	}
	if got := claude429ScopeFor(model, nil); got != claude429Model {
		t.Fatalf("model scope = %v, want model", got)
	}
	fast := http.Header{}
	if got := claude429ScopeFor(fast, []byte("Usage credits are required for fast mode")); got != claude429Request {
		t.Fatalf("fast scope = %v, want request", got)
	}
}

func TestClaudeFastEntitlement429DoesNotCoolModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"Usage credits are required for fast mode"}`)
	}))
	t.Cleanup(upstream.Close)
	account := testAccount("account-uuid", 10)
	srv := newTestServer(t, []*Account{account})
	srv.upstream = upstream.URL
	srv.lookupAPIKey = func(string) (string, bool, error) { return "test", true, nil }
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","service_tier":"fast","messages":[]}`))
	request.Header.Set("Authorization", "Bearer test")
	response := httptest.NewRecorder()
	srv.claudeMessagesHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if got := account.modelCooldownUntil("claude-sonnet"); !got.IsZero() {
		t.Fatalf("model cooldown = %s, want none", got)
	}
}

func TestClaudeStreamRetriesBeforeBootstrapOutput(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	}))
	t.Cleanup(upstream.Close)
	srv := newTestServer(t, []*Account{testAccount("account-a", 10), testAccount("account-b", 10)})
	srv.upstream = upstream.URL
	srv.lookupAPIKey = func(string) (string, bool, error) { return "test", true, nil }
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer test")
	response := httptest.NewRecorder()
	srv.claudeMessagesHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want retry before first stream event", calls)
	}
	if !strings.Contains(response.Body.String(), "message_start") {
		t.Fatalf("response = %q, want bootstrap event", response.Body.String())
	}
}

func TestClaudeStreamDoesNotRetryAfterBootstrapOutput(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	}))
	t.Cleanup(upstream.Close)
	srv := newTestServer(t, []*Account{testAccount("account-a", 10), testAccount("account-b", 10)})
	srv.upstream = upstream.URL
	srv.lookupAPIKey = func(string) (string, bool, error) { return "test", true, nil }
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer test")
	response := httptest.NewRecorder()
	srv.claudeMessagesHTTP(response, request)
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status = %d calls = %d, want 200 and one attempt", response.Code, calls)
	}
}
