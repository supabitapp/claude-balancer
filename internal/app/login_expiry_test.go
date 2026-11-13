package app

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func useClaudeTokenResponse(t *testing.T, body string) *http.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	oldToken, oldProfile := claudeOAuthTokenURL, claudeOAuthProfileURL
	claudeOAuthTokenURL = server.URL + "/token"
	claudeOAuthProfileURL = server.URL + "/profile"
	t.Cleanup(func() { claudeOAuthTokenURL, claudeOAuthProfileURL = oldToken, oldProfile })
	return server.Client()
}

func TestClaudeSignInRecordsReportedLoginExpiry(t *testing.T) {
	for name, test := range map[string]struct {
		reported string
		lifetime time.Duration
	}{
		"reported lifetime sets the limit":        {reported: `,"refresh_token_expires_in":2419200`, lifetime: 28 * 24 * time.Hour},
		"missing lifetime assumes 30 days":        {lifetime: 30 * 24 * time.Hour},
		"malformed lifetime assumes 30 days":      {reported: `,"refresh_token_expires_in":"soon"`, lifetime: 30 * 24 * time.Hour},
		"overflowing lifetime assumes 30 days":    {reported: `,"refresh_token_expires_in":1e309`, lifetime: 30 * 24 * time.Hour},
		"implausible lifetime assumes 30 days":    {reported: `,"refresh_token_expires_in":10000000000`, lifetime: 30 * 24 * time.Hour},
		"negative lifetime assumes 30 days":       {reported: `,"refresh_token_expires_in":-60`, lifetime: 30 * 24 * time.Hour},
		"lifetime past ten years assumes 30 days": {reported: `,"refresh_token_expires_in":320000000`, lifetime: 30 * 24 * time.Hour},
		"lifetime under ten years sets the limit": {reported: `,"refresh_token_expires_in":31536000`, lifetime: 365 * 24 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			client := useClaudeTokenResponse(t, `{"access_token":"access","refresh_token":"refresh","expires_in":28800,"account":{"uuid":"account"},"organization":{"uuid":"org"}`+test.reported+`}`)
			before := time.Now()

			credentials, err := exchangeClaudeCode(context.Background(), client, "code", "state", claudePKCE{verifier: "verifier"}, "http://localhost:54545/callback")
			if err != nil {
				t.Fatal(err)
			}

			got := accountFromClaudeCredentials(credentials).loginExpires()
			if want := before.Add(test.lifetime); got.Before(want) || got.After(want.Add(time.Minute)) {
				t.Fatalf("login expiry = %s, want about %s", got, want)
			}
		})
	}
}

func TestClaudeRefreshNeverExtendsLoginExpiry(t *testing.T) {
	signedIn := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	for name, test := range map[string]struct {
		signedIn time.Time
		reported string
		want     time.Time
		within   time.Duration
	}{
		"later report keeps the sign-in limit":       {signedIn: signedIn, reported: `,"refresh_token_expires_in":2592000`, want: signedIn},
		"missing report keeps the sign-in limit":     {signedIn: signedIn, want: signedIn},
		"malformed report keeps the sign-in limit":   {signedIn: signedIn, reported: `,"refresh_token_expires_in":"soon"`, want: signedIn},
		"overflowing report keeps the sign-in limit": {signedIn: signedIn, reported: `,"refresh_token_expires_in":1e309`, want: signedIn},
		"earlier report shortens the sign-in limit":  {signedIn: signedIn, reported: `,"refresh_token_expires_in":3600`, want: time.Now().Add(time.Hour), within: time.Minute},
		"unknown sign-in limit stays unknown":        {reported: `,"refresh_token_expires_in":2592000`},
	} {
		t.Run(name, func(t *testing.T) {
			client := useClaudeTokenResponse(t, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800`+test.reported+`}`)
			state := accountState{
				IDToken:      claudeIdentityToken(claudeCredentials{AccountID: "account", OrganizationID: "org", LoginExpiresAt: test.signedIn}),
				RefreshToken: "old-refresh-" + name,
			}

			tokens, permanent, err := exchangeAccountRefreshToken(context.Background(), client, state)
			if err != nil || permanent {
				t.Fatalf("refresh = %v (permanent=%v)", err, permanent)
			}
			if tokens.RefreshToken != "new-refresh" {
				t.Fatalf("refresh token = %q, want the rotated token", tokens.RefreshToken)
			}

			got := accountFromState(accountState{IDToken: tokens.IDToken}).loginExpires()
			if difference := got.Sub(test.want).Abs(); difference > test.within {
				t.Fatalf("login expiry = %s, want %s within %s", got, test.want, test.within)
			}
		})
	}
}

func TestClaudePlanUpdateKeepsLoginExpiry(t *testing.T) {
	expires := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	account := accountFromClaudeCredentials(&claudeCredentials{AccountID: "account", PlanType: "claude_pro", LoginExpiresAt: expires})

	if !account.setPlanType("claude_max") {
		t.Fatal("plan update reported no change")
	}

	if got := account.loginExpires(); !got.Equal(expires) {
		t.Fatalf("login expiry after plan update = %s, want %s", got, expires)
	}
}

var (
	dashboardAccountRow = regexp.MustCompile(`(?s)<tr[^>]*\bid="(?:account|workspace)-[^"]*"[^>]*>.*?</tr>`)
	dashboardTooltip    = regexp.MustCompile(`data-tooltip="([^"]*)"`)
	htmlTag             = regexp.MustCompile(`<[^>]*>`)
)

func TestDashboardWarnsBeforeLoginExpires(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		expires  time.Duration
		wantNote string
		wantInfo string
	}{
		"unknown limit shows nothing": {},
		"distant limit stays in tooltip": {expires: 20 * 24 * time.Hour,
			wantInfo: "Sign-in ends between 2026-11-30 and 2026-12-03 UTC."},
		"approaching earliest end warns the row": {expires: 8 * 24 * time.Hour, wantNote: "sign-in may end in 5d0h",
			wantInfo: "Sign-in ends between 2026-11-18 and 2026-11-21 UTC. Sign in again at /accounts to keep this account available."},
		"inside the early window warns the row": {expires: 50 * time.Hour, wantNote: "sign-in can end any time",
			wantInfo: "Sign-in can end at any time until 2026-11-15 14:00 UTC. Sign in again at /accounts to keep this account available."},
		"passed limit says it may be over": {expires: -time.Hour, wantNote: "sign-in may have ended",
			wantInfo: "Sign-in may have ended between 2026-11-10 and 2026-11-13 UTC. Sign in again at /accounts."},
	} {
		t.Run(name, func(t *testing.T) {
			known := test.wantInfo != ""
			var accounts []*Account
			for id, plan := range map[string]string{"routed": "claude_max", "workspace": "enterprise"} {
				credentials := claudeCredentials{AccountID: id, PlanType: plan}
				if known {
					credentials.LoginExpiresAt = now.Add(test.expires)
				}
				accounts = append(accounts, accountFromClaudeCredentials(&credentials))
			}
			server := &server{pool: &Pool{accounts: accounts}, stats: newStatsWithPrices(priceSnapshot{})}

			page, err := renderDashboard("page", server.currentDashboard(now))
			if err != nil {
				t.Fatal(err)
			}
			rows := dashboardAccountRow.FindAllString(string(page), -1)
			if len(rows) != len(accounts) {
				t.Fatalf("dashboard rows = %d, want %d", len(rows), len(accounts))
			}
			for _, row := range rows {
				visible := html.UnescapeString(htmlTag.ReplaceAllString(row, " "))
				if test.wantNote == "" && strings.Contains(visible, "sign-in") || !strings.Contains(visible, test.wantNote) {
					t.Fatalf("row text = %q, want sign-in warning %q", strings.Join(strings.Fields(visible), " "), test.wantNote)
				}
				var tooltips []string
				for _, match := range dashboardTooltip.FindAllStringSubmatch(row, -1) {
					tooltips = append(tooltips, html.UnescapeString(match[1]))
				}
				hover := strings.Join(tooltips, "\n")
				if !known && strings.Contains(hover, "Sign-in") || !strings.Contains(hover, test.wantInfo) {
					t.Fatalf("row tooltips = %q, want sign-in info %q", hover, test.wantInfo)
				}
			}

			payload, err := json.Marshal(server.currentStats(now))
			if err != nil {
				t.Fatal(err)
			}
			var stats struct {
				Accounts []struct {
					LoginExpiresAt *time.Time `json:"login_expires_at"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(payload, &stats); err != nil {
				t.Fatal(err)
			}
			if len(stats.Accounts) != len(accounts) {
				t.Fatalf("stats accounts = %d, want %d", len(stats.Accounts), len(accounts))
			}
			for _, account := range stats.Accounts {
				switch got := account.LoginExpiresAt; {
				case !known && got != nil:
					t.Fatalf("stats login_expires_at = %s, want it omitted", got)
				case known && (got == nil || !got.Equal(now.Add(test.expires))):
					t.Fatalf("stats login_expires_at = %v, want %s", got, now.Add(test.expires))
				}
			}
		})
	}
}
