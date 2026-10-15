package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPollClaudePlanPersistsProfileTier(t *testing.T) {
	profile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"account":{"uuid":"account"},"organization":{"rate_limit_tier":"default_claude_max_20x"}}`)
	}))
	t.Cleanup(profile.Close)
	previous := claudeOAuthProfileURL
	claudeOAuthProfileURL = profile.URL
	t.Cleanup(func() { claudeOAuthProfileURL = previous })

	account := accountFromState(accountState{
		IDToken:     claudeIdentityToken(claudeCredentials{AccountID: "account"}),
		AccessToken: "access-token",
		LastRefresh: time.Now(),
	})
	srv := newTestServer(t, []*Account{account})
	if err := srv.pollClaudePlan(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if got := srv.pool.all()[0].plan(); got != "default_claude_max_20x" {
		t.Fatalf("pool plan = %q, want default_claude_max_20x", got)
	}
	if got := claudeCredentialsFromIdentityToken(srv.pool.all()[0].persisted().IDToken).PlanType; got != "default_claude_max_20x" {
		t.Fatalf("persisted plan = %q, want default_claude_max_20x", got)
	}
}
