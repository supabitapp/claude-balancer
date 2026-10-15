package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		"Anthropic-Beta":    "oauth-2025-04-20",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"X-App": "cli",
	} {
		if got := seen.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
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
	if snapshot.MonthlyUsage.InputTokens != 11 || snapshot.MonthlyUsage.OutputTokens != 4 || snapshot.MonthlyUsage.InputDetails.CachedTokens != 3 || snapshot.MonthlyUsage.InputDetails.CacheWriteTokens != 2 {
		t.Fatalf("monthly usage = %+v, want input 11 cached 3 cache-write 2 output 4", snapshot.MonthlyUsage)
	}
}

func TestClaudeResponseUsageParsesStream(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11,\"cache_read_input_tokens\":3}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n"
	usage := claudeResponseUsage([]byte(body))
	if usage.InputTokens != 11 || usage.OutputTokens != 4 || usage.InputDetails.CachedTokens != 3 || usage.TotalTokens != 15 {
		t.Fatalf("stream usage = %+v, want input 11 cached 3 output 4 total 15", usage)
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
