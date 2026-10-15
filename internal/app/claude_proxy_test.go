package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
