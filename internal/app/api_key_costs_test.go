package app

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func ledgerTestPrices(inputRate int64, fetchedAt time.Time) priceSnapshot {
	return newPriceSnapshot(map[string]modelPrice{
		"claude-test": {standard: []priceTier{{rates: tokenRates{input: inputRate}}}},
	}, fetchedAt)
}

func TestAdminAPIKeyCostsFollowNewUsageAndPrices(t *testing.T) {
	srv := newTestServer(t, nil)
	enableTestAdmin(t, srv)
	now := time.Now()
	srv.prices.install(ledgerTestPrices(3_000, now))
	if err := srv.pool.store.addAPIKey(storedAPIKey{Name: "client", Secret: "client-secret", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	record := func(at time.Time, apiKey string) {
		t.Helper()
		usage := storedUsage{At: at, APIKeyName: apiKey, Model: "claude-test", Usage: responseUsage{InputTokens: 1_000_000}}
		if err := srv.pool.store.recordUsage(usage); err != nil {
			t.Fatal(err)
		}
	}
	record(calendarMonthStart(now).Add(-time.Hour), "client")
	record(now, "client")
	record(now, "")
	handler := srv.routes()
	cookie, _ := loginTestAdmin(t, handler)
	rowPattern := regexp.MustCompile(`(?s)<tr id="` + dashboardDOMID("key", "client") + `">(.*?)</tr>`)
	checkCosts := func(month, lifetime string) {
		t.Helper()
		response := adminRequest(handler, http.MethodGet, "/admin", nil, cookie)
		if response.Code != http.StatusOK {
			t.Fatalf("admin status = %d", response.Code)
		}
		row := rowPattern.FindStringSubmatch(response.Body.String())
		want := "<td>" + month + "</td>\n<td>" + lifetime + "</td>"
		if row == nil || !strings.Contains(row[1], want) {
			t.Fatalf("client row = %q, want costs %q", row, want)
		}
	}
	checkCosts("$3.00", "$6.00")
	record(now, "client")
	checkCosts("$6.00", "$9.00")
	srv.prices.install(ledgerTestPrices(6_000, now.Add(time.Hour)))
	checkCosts("$12.00", "$18.00")
}

func TestAPIKeyLedgerRollsMonthOver(t *testing.T) {
	store, err := openStateStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.Local)
	if err := store.addAPIKey(storedAPIKey{Name: "client", Secret: "secret", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.recordUsage(storedUsage{At: now, APIKeyName: "client", Model: "claude-test", Usage: responseUsage{InputTokens: 1_000}}); err != nil {
		t.Fatal(err)
	}
	prices := ledgerTestPrices(3_000, now)
	var ledger apiKeyLedger
	for _, test := range []struct {
		at         time.Time
		monthInput int64
		monthCost  int64
	}{
		{at: now, monthInput: 1_000, monthCost: 3_000_000},
		{at: time.Date(2026, time.November, 1, 0, 30, 0, 0, time.Local), monthInput: 0, monthCost: 0},
	} {
		totals, err := ledger.totals(store, prices, test.at)
		if err != nil {
			t.Fatal(err)
		}
		got := totals["client"]
		if got.month.InputTokens != test.monthInput || got.monthCost.apiCostNanoDollars != test.monthCost ||
			got.lifetimeCost.apiCostNanoDollars != 3_000_000 || !got.lastUsed.Equal(now) {
			t.Fatalf("totals at %v = %+v", test.at, got)
		}
	}
}
