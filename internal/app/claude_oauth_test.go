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
		requestBody = string(body)
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
