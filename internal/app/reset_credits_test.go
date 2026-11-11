package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type resetCreditUpstream struct {
	t       *testing.T
	grants  string
	result  string
	claims  []consumeResetCreditRequest
	polling []string
}

func (u *resetCreditUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("anthropic-beta") != claudeOAuthBeta {
		u.t.Errorf("unexpected credentials on %s %s", r.Method, r.URL)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/oauth/usage" && r.URL.Query().Has(resetCreditProgram):
		u.polling = append(u.polling, r.URL.RawQuery)
		io.WriteString(w, u.grants)
	case r.Method == http.MethodGet:
		io.WriteString(w, `{}`)
	case r.Method == http.MethodPost && r.URL.Path == "/api/organizations/org-1/reset_rate_limits":
		var claim consumeResetCreditRequest
		if err := json.NewDecoder(r.Body).Decode(&claim); err != nil {
			u.t.Error(err)
		}
		u.claims = append(u.claims, claim)
		u.grants = `{"cedar_ember":{"eligible":true,"grants":[]}}`
		io.WriteString(w, u.result)
	default:
		u.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func newResetCreditServer(t *testing.T, upstream *resetCreditUpstream) (*server, *Account) {
	t.Helper()
	upstream.t = t
	api := httptest.NewServer(upstream)
	t.Cleanup(api.Close)
	previousAccount, previousOrganization, previousProfile := accountAPIBaseURL, organizationAPIBaseURL, claudeOAuthProfileURL
	accountAPIBaseURL = api.URL + "/api/oauth"
	organizationAPIBaseURL = api.URL + "/api/organizations"
	claudeOAuthProfileURL = api.URL + "/api/oauth/profile"
	t.Cleanup(func() {
		accountAPIBaseURL, organizationAPIBaseURL, claudeOAuthProfileURL = previousAccount, previousOrganization, previousProfile
	})
	account := accountFromState(accountState{
		IDToken:     claudeIdentityToken(claudeCredentials{AccountID: "account", OrganizationID: "org-1", Email: "a@example.com"}),
		AccessToken: "access-token",
		LastRefresh: time.Now(),
	})
	return newTestServer(t, []*Account{account}), account
}

func useBankedReset(srv *server, account *Account, credit string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/admin/accounts/reset", nil)
	request.PostForm = url.Values{"account": {account.id()}, "credit": {credit}}
	recorder := httptest.NewRecorder()
	srv.adminBankedReset(recorder, request, adminSession{}, account)
	return recorder
}

const testResetGrants = `{"cedar_ember":{"eligible":true,"next_grant_id":"weekly_b","grants":[
	{"id":"session_a","label":"5-hour reset","resets_left":2,"ends_at":"2099-01-02T00:00:00Z","usable_now":false},
	{"id":"weekly_b","label":"Weekly reset","resets_left":1,"ends_at":"2099-01-01T00:00:00+00:00","usable_now":true},
	{"id":"paused_c","label":"Paused reset","resets_left":4,"paused":true}
]}}`

func TestPollResetCreditsOffersTheNextUsableGrant(t *testing.T) {
	upstream := &resetCreditUpstream{grants: testResetGrants}
	srv, account := newResetCreditServer(t, upstream)
	if err := srv.pollResetCredits(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	if len(upstream.polling) != 1 || upstream.polling[0] != "cedar_ember=1&skip_spend=1" {
		t.Fatalf("poll queries = %q", upstream.polling)
	}
	now := time.Now()
	stats := srv.currentStats(now).Accounts[0]
	if stats.BankedResets == nil || *stats.BankedResets != 3 {
		t.Fatalf("banked resets = %v, want 3", stats.BankedResets)
	}
	if got := dashboardResetCreditID(now, stats.ResetCredits); got != "weekly_b" {
		t.Fatalf("offered grant = %q, want weekly_b", got)
	}
	if info := dashboardResetInfo(now, stats.ResetCredits); !strings.Contains(info, "1 × Weekly reset. Expires in") || strings.Contains(info, "Paused") {
		t.Fatalf("tooltip = %q", info)
	}
	page := httptest.NewRecorder()
	srv.renderAdmin(page, httptest.NewRequest(http.MethodGet, "/admin", nil), adminSession{}, "", "", http.StatusOK)
	if !strings.Contains(page.Body.String(), `name="credit" value="weekly_b"`) {
		t.Fatalf("admin page offers no reset button: %s", page.Body.String())
	}
}

func TestPollResetCreditsHidesIneligibleAccounts(t *testing.T) {
	for _, body := range []string{`{}`, `{"cedar_ember":{"eligible":false,"ineligible_reason":"tier"}}`} {
		srv, account := newResetCreditServer(t, &resetCreditUpstream{grants: body})
		if err := srv.pollResetCredits(t.Context(), account); err != nil {
			t.Fatal(err)
		}
		if stats := srv.currentStats(time.Now()).Accounts[0]; stats.BankedResets != nil {
			t.Fatalf("banked resets for %s = %d, want unknown", body, *stats.BankedResets)
		}
	}
}

func TestAdminBankedResetRedeemsTheChosenGrant(t *testing.T) {
	upstream := &resetCreditUpstream{grants: testResetGrants, result: `{"result":"reset","resets_left":0}`}
	srv, account := newResetCreditServer(t, upstream)
	recorder := useBankedReset(srv, account, "weekly_b")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Banked reset applied.") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if len(upstream.claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(upstream.claims))
	}
	claim := upstream.claims[0]
	if claim.Program != "cedar_ember" || claim.GrantID != "weekly_b" || claim.RequestID == "" {
		t.Fatalf("claim = %+v", claim)
	}
	if stats := srv.currentStats(time.Now()).Accounts[0]; stats.BankedResets == nil || *stats.BankedResets != 0 {
		t.Fatalf("banked resets after use = %v, want 0", stats.BankedResets)
	}
}

func TestAdminBankedResetReportsRefusals(t *testing.T) {
	for _, test := range []struct {
		name, credit, result, notice string
		status, claims               int
	}{
		{name: "stale grant", credit: "gone", notice: "no longer available", status: http.StatusConflict},
		{name: "paused grant", credit: "paused_c", notice: "no longer available", status: http.StatusConflict},
		{name: "not limited", credit: "weekly_b", result: `{"result":"not_limited"}`, notice: "has not reached a limit", status: http.StatusConflict, claims: 1},
		{name: "cooldown", credit: "weekly_b", result: `{"result":"cooldown"}`, notice: "cooling down", status: http.StatusConflict, claims: 1},
		{name: "unavailable", credit: "weekly_b", result: `{"result":"unavailable"}`, notice: "Could not confirm", status: http.StatusBadGateway, claims: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &resetCreditUpstream{grants: testResetGrants, result: test.result}
			srv, account := newResetCreditServer(t, upstream)
			recorder := useBankedReset(srv, account, test.credit)
			if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.notice) {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
			if len(upstream.claims) != test.claims {
				t.Fatalf("claims = %d, want %d", len(upstream.claims), test.claims)
			}
		})
	}
}
