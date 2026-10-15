package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type testLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *testLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *testLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func captureTestLogs(server *server) *testLogBuffer {
	logs := &testLogBuffer{}
	server.log = slog.New(slog.NewJSONHandler(logs, nil))
	return logs
}

func useOAuthRefreshServer(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"refreshed-token","refresh_token":"refreshed-refresh"}`)
	}))
	previous := oauthEndpoint
	oauthEndpoint = oauth.URL
	t.Cleanup(func() {
		oauthEndpoint = previous
		oauth.Close()
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func newTestServer(t *testing.T, accounts []*Account) *server {
	t.Helper()
	state, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := loadPool(state)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := pool.add(account); err != nil {
			state.Close()
			t.Fatal(err)
		}
	}
	stats, err := newPersistentStats(state, testPriceSnapshot(t), nil)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	prices := newPriceCatalog()
	prices.install(testPriceSnapshot(t))
	srv := &server{
		ctx:      ctx,
		pool:     pool,
		stats:    stats,
		prices:   prices,
		upstream: "http://127.0.0.1:1",
		client:   newProxyClient(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	t.Cleanup(func() {
		srv.refreshes.mu.Lock()
		alreadyStopped := srv.refreshes.stopping
		srv.refreshes.mu.Unlock()
		if err := srv.stopRefreshes(cancel); err != nil && !alreadyStopped {
			t.Error(err)
		}
		state.Close()
	})
	return srv
}

func waitHTTPSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for HTTP test synchronization")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testAccountWithCredits(id string) *Account {
	account := testAccount(id, 0)
	account.credits = &creditsPayload{HasCredits: true, Balance: "1"}
	return account
}

func testAccount(id string, used float64) *Account {
	return testAccountWithPlan(id, used, "pro")
}

func testAccountWithPlan(id string, used float64, plan string) *Account {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"provider":"claude","email":"%s@example.com","account_id":"%s"}`, id, id)))
	account := accountFromState(accountState{
		IDToken:      "x." + payload + ".x",
		AccessToken:  "token-" + id,
		RefreshToken: "refresh-" + id,
		LastRefresh:  time.Now(),
	})
	account.planType = plan
	account.primary = window{usedPercent: used, seenAt: time.Now()}
	account.secondary = window{usedPercent: used, seenAt: time.Now()}
	return account
}

func adoptTestResetCredit(account *Account, expiresAt time.Time) {
	account.adoptResetCredits(time.Now(), 1, []resetCredit{{
		ID:        "credit-" + account.id(),
		ResetType: "claude_rate_limits",
		Status:    "available",
		ExpiresAt: &expiresAt,
	}})
}

func setTestAccountUsage(account *Account, used float64) {
	account.mu.Lock()
	defer account.mu.Unlock()
	account.primary.usedPercent = used
	account.secondary.usedPercent = used
}

func setTestAccountSpent(account *Account, spent bool) {
	account.mu.Lock()
	defer account.mu.Unlock()
	account.spent = spent
}

const testAdminPassword = "local-test-password-only"

func enableTestAdmin(t *testing.T, s *server) string {
	t.Helper()
	hash, err := hashAdminPassword([]byte(testAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pool.store.raw.SetAdminPasswordHash(hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func adminRequest(h http.Handler, method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://balancer.test"+path, strings.NewReader(form.Encode()))
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}

func adminFormCSRF(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(response.Body.String())
	if len(match) != 2 {
		t.Fatalf("missing CSRF field; status=%d", response.Code)
	}
	return match[1]
}

func loginTestAdmin(t *testing.T, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	login := adminRequest(h, http.MethodGet, "/admin/login", nil)
	response := adminRequest(h, http.MethodPost, "/admin/login", url.Values{"csrf": {adminFormCSRF(t, login)}, "password": {testAdminPassword}}, login.Result().Cookies()...)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login status=%d: %s", response.Code, response.Body.String())
	}
	var cookie *http.Cookie
	for _, candidate := range response.Result().Cookies() {
		if candidate.Name == "__Host-cb-admin" {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("missing session cookie")
	}
	page := adminRequest(h, http.MethodGet, "/admin", nil, cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("admin status=%d", page.Code)
	}
	return cookie, adminFormCSRF(t, page)
}

func testPriceSnapshot(t *testing.T) priceSnapshot {
	t.Helper()
	return newPriceSnapshot(nil, time.Now())
}
