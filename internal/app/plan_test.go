package app

import (
	"context"
	"encoding/json"
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

func TestClaudeProfilePlanMarksAccountsWithoutSubscriptionFree(t *testing.T) {
	for _, test := range []struct {
		name    string
		profile string
		want    string
	}{
		{name: "max", profile: `{"account":{"has_claude_max":true},"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`, want: "default_claude_max_20x"},
		{name: "pro", profile: `{"account":{"has_claude_pro":true},"organization":{"organization_type":"claude_pro"}}`, want: "claude_pro"},
		{name: "team", profile: `{"organization":{"organization_type":"claude_team","rate_limit_tier":"default_claude_team"}}`, want: "default_claude_team"},
		{name: "no subscription", profile: `{"organization":{"organization_type":"claude_free","rate_limit_tier":"default_claude_ai"}}`, want: "free"},
		{name: "unknown type with subscription flag", profile: `{"account":{"has_claude_max":true},"organization":{"organization_type":"claude_max_plus"}}`, want: "claude_max_plus"},
		{name: "missing organization type", profile: `{"organization":{"rate_limit_tier":"default_claude_max_5x"}}`, want: "default_claude_max_5x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var profile claudeOAuthProfile
			if err := json.Unmarshal([]byte(test.profile), &profile); err != nil {
				t.Fatal(err)
			}
			if got := claudeProfilePlan(&profile); got != test.want {
				t.Fatalf("plan = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFreeAccountsAreExcludedFromRouting(t *testing.T) {
	free := testAccountWithPlan("free", 0, "free")
	paid := testAccountWithPlan("paid", 80, "default_claude_max_20x")
	pool := &Pool{accounts: []*Account{free, paid}}

	if got := free.routingCandidate().status(time.Now()); got != accountNotRouted {
		t.Fatalf("free account status = %s, want %s", got, accountNotRouted)
	}
	if got := pool.route(nil, nil).account; got != paid {
		t.Fatalf("fresh route = %v, want paid account", got)
	}
	if got := pool.route([]string{"free"}, nil).account; got != paid {
		t.Fatalf("owned route = %v, want paid account", got)
	}
	if got := pool.route(nil, map[string]bool{"paid": true}).account; got != nil {
		t.Fatalf("route with only a free account = %v, want none", got.id())
	}
}
