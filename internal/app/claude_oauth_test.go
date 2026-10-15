package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewClaudePKCE(t *testing.T) {
	pkce, err := newClaudePKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkce.verifier) < 43 || len(pkce.verifier) > 128 || pkce.challenge == "" {
		t.Fatalf("unexpected PKCE values: verifier=%d challenge=%q", len(pkce.verifier), pkce.challenge)
	}
	hash := sha256.Sum256([]byte(pkce.verifier))
	want := base64.RawURLEncoding.EncodeToString(hash[:])
	if pkce.challenge != want {
		t.Fatalf("challenge = %q, want %q", pkce.challenge, want)
	}
}

func TestClaudeAuthorizationURL(t *testing.T) {
	pkce := claudePKCE{challenge: "challenge"}
	parsed, err := url.Parse(claudeAuthorizationURL("state", pkce, "http://localhost:54545/callback"))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"client_id":             claudeOAuthClientID,
		"response_type":         "code",
		"redirect_uri":          "http://localhost:54545/callback",
		"scope":                 claudeOAuthScope,
		"code_challenge":        "challenge",
		"code_challenge_method": "S256",
		"state":                 "state",
		"code":                  "true",
	} {
		if query.Get(key) != want {
			t.Fatalf("query %s = %q, want %q", key, query.Get(key), want)
		}
	}
}

func TestExchangeClaudeCodeUsesNativeRequestAndProfileMetadata(t *testing.T) {
	var tokenBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("token request = %s %s content-type=%q", r.Method, r.URL, r.Header.Get("Content-Type"))
			}
			if err := json.NewDecoder(r.Body).Decode(&tokenBody); err != nil {
				t.Fatal(err)
			}
			io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"account":{"uuid":"token-account","email_address":"token@example.com"},"organization":{"uuid":"token-org","name":"Token Org"}}`)
			return
		}
		if r.URL.Path == "/profile" {
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Fatalf("profile authorization = %q", r.Header.Get("Authorization"))
			}
			io.WriteString(w, `{"account":{"uuid":"profile-account","email":"profile@example.com"},"organization":{"uuid":"profile-org","name":"Profile Org"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	oldToken, oldProfile := claudeOAuthTokenURL, claudeOAuthProfileURL
	claudeOAuthTokenURL = server.URL + "/token"
	claudeOAuthProfileURL = server.URL + "/profile"
	t.Cleanup(func() { claudeOAuthTokenURL, claudeOAuthProfileURL = oldToken, oldProfile })

	credentials, err := exchangeClaudeCode(context.Background(), server.Client(), "code#fragment-state", "query-state", claudePKCE{verifier: "verifier"}, "http://localhost:54545/callback")
	if err != nil {
		t.Fatal(err)
	}
	if got := tokenBody["state"]; got != "fragment-state" {
		t.Fatalf("state = %v, want fragment-state", got)
	}
	if got := tokenBody["grant_type"]; got != "authorization_code" {
		t.Fatalf("grant_type = %v", got)
	}
	if credentials.AccountID != "profile-account" || credentials.Email != "profile@example.com" || credentials.OrganizationID != "profile-org" || credentials.OrganizationName != "Profile Org" {
		t.Fatalf("profile metadata = %+v", credentials)
	}
	if credentials.ExpiresAt.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expiry = %s", credentials.ExpiresAt)
	}
}

func TestRefreshClaudeTokenKeepsRotatingTokenFallback(t *testing.T) {
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/token" {
			requestBody = string(body)
		}
		io.WriteString(w, `{"access_token":"new-access","expires_in":1800}`)
	}))
	t.Cleanup(server.Close)
	oldToken, oldProfile := claudeOAuthTokenURL, claudeOAuthProfileURL
	claudeOAuthTokenURL = server.URL + "/token"
	claudeOAuthProfileURL = server.URL + "/profile"
	t.Cleanup(func() { claudeOAuthTokenURL, claudeOAuthProfileURL = oldToken, oldProfile })

	credentials, err := refreshClaudeToken(context.Background(), server.Client(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessToken != "new-access" || credentials.RefreshToken != "old-refresh" {
		t.Fatalf("credentials = %+v", credentials)
	}
	if !strings.Contains(requestBody, `"grant_type":"refresh_token"`) || !strings.Contains(requestBody, `"refresh_token":"old-refresh"`) {
		t.Fatalf("refresh body = %s", requestBody)
	}
}

func TestExchangeAccountRefreshTokenUsesClaudeIssuerAndPreservesIdentity(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
				t.Fatal(err)
			}
			io.WriteString(w, `{"access_token":"new-access","expires_in":3600}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	oldToken, oldProfile := claudeOAuthTokenURL, claudeOAuthProfileURL
	claudeOAuthTokenURL = server.URL + "/token"
	claudeOAuthProfileURL = server.URL + "/profile"
	t.Cleanup(func() { claudeOAuthTokenURL, claudeOAuthProfileURL = oldToken, oldProfile })

	state := accountState{
		IDToken:      claudeIdentityToken(claudeCredentials{AccountID: "account", Email: "user@example.com", OrganizationID: "org", OrganizationName: "Org", DeviceID: strings.Repeat("a", 64)}),
		RefreshToken: "old-refresh",
	}
	tokens, permanent, err := exchangeAccountRefreshToken(context.Background(), server.Client(), state)
	if err != nil || permanent {
		t.Fatalf("refresh = %v (permanent=%v)", err, permanent)
	}
	if tokens.AccessToken != "new-access" || tokens.RefreshToken != "old-refresh" {
		t.Fatalf("tokens = %+v", tokens)
	}
	identity := claudeCredentialsFromIdentityToken(tokens.IDToken)
	if identity.AccountID != "account" || identity.Email != "user@example.com" || identity.OrganizationID != "org" || identity.DeviceID != strings.Repeat("a", 64) {
		t.Fatalf("identity = %+v", identity)
	}
	if requestBody["client_id"] != claudeOAuthClientID || requestBody["grant_type"] != "refresh_token" || requestBody["refresh_token"] != "old-refresh" || requestBody["scope"] != claudeOAuthScope {
		t.Fatalf("request body = %#v", requestBody)
	}
}

func TestExchangeAccountRefreshTokenMarksClaudeInvalidGrantPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	t.Cleanup(server.Close)
	oldToken, oldProfile := claudeOAuthTokenURL, claudeOAuthProfileURL
	claudeOAuthTokenURL = server.URL
	claudeOAuthProfileURL = server.URL
	t.Cleanup(func() { claudeOAuthTokenURL, claudeOAuthProfileURL = oldToken, oldProfile })

	state := accountState{IDToken: claudeIdentityToken(claudeCredentials{AccountID: "account"}), RefreshToken: "old-refresh"}
	_, permanent, err := exchangeAccountRefreshToken(context.Background(), server.Client(), state)
	if err == nil || !permanent || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("refresh = %v (permanent=%v), want permanent invalid_grant", err, permanent)
	}
}

func TestRefreshClaudeTokenBlocksRetryAfterRateLimit(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate_limited"}`)
	}))
	t.Cleanup(server.Close)
	oldToken, oldProfile := claudeOAuthTokenURL, claudeOAuthProfileURL
	claudeOAuthTokenURL = server.URL
	claudeOAuthProfileURL = server.URL
	t.Cleanup(func() {
		claudeOAuthTokenURL, claudeOAuthProfileURL = oldToken, oldProfile
		claudeRefreshBlockMu.Lock()
		delete(claudeRefreshBlocked, "rate-limited-refresh")
		claudeRefreshBlockMu.Unlock()
	})

	_, firstErr := refreshClaudeToken(context.Background(), server.Client(), "rate-limited-refresh")
	if firstErr == nil {
		t.Fatal("first refresh unexpectedly succeeded")
	}
	_, secondErr := refreshClaudeToken(context.Background(), server.Client(), "rate-limited-refresh")
	if secondErr == nil || !strings.Contains(secondErr.Error(), "temporarily blocked") {
		t.Fatalf("second refresh = %v, want temporary block", secondErr)
	}
	if calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", calls)
	}
}
