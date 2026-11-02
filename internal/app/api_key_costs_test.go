package app

import (
	"testing"
	"time"
)

func ledgerTestPrices(inputRate int64, fetchedAt time.Time) priceSnapshot {
	return newPriceSnapshot(map[string]modelPrice{
		"claude-test": {standard: []priceTier{{rates: tokenRates{input: inputRate}}}},
	}, fetchedAt)
}

func TestAdminKeyCostsFollowUsagePricesAndMonths(t *testing.T) {
	srv := newTestServer(t, nil)
	now := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.Local)
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
	check := func(at time.Time, month, lifetime, lastUsed string) {
		t.Helper()
		views, err := srv.adminKeys(at)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 1 || views[0].Cost != month || views[0].LifetimeCost != lifetime || views[0].LastUsed != lastUsed {
			t.Fatalf("adminKeys(%v) = %+v, want month %s lifetime %s last used %s", at, views, month, lifetime, lastUsed)
		}
	}
	record(calendarMonthStart(now).Add(-time.Hour), "client")
	record(now, "client")
	record(now, "")
	check(now, "$3.00", "$6.00", "just now")
	record(now, "client")
	check(now, "$6.00", "$9.00", "just now")
	srv.prices.install(ledgerTestPrices(6_000, now.Add(time.Hour)))
	check(now, "$12.00", "$18.00", "just now")
	check(time.Date(2026, time.November, 1, 0, 30, 0, 0, time.Local), "$0.00", "$18.00", "12 hours ago")
}
