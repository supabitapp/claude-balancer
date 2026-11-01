package app

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func lastMonthTestPrices(t *testing.T) priceSnapshot {
	t.Helper()
	snapshot, err := parseModelsDevPriceCatalog([]byte(`{"anthropic":{"models":{
		"claude-opus-test":{"cost":{"input":15,"output":75}},
		"claude-sonnet-test":{"cost":{"input":3,"output":15}}
	}}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDashboardMonthlyCostShowsLastMonth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		stats := newStatsWithPrices(lastMonthTestPrices(t))
		lastMonth := calendarMonthStart(now).Add(-time.Hour)
		stats.applyUsageAt(lastMonth, "", "account", "claude-opus-test", "", "", responseUsage{InputTokens: 100_000})
		stats.applyUsageAt(lastMonth, "", "account", "claude-sonnet-test", "", "", responseUsage{InputTokens: 100_000})
		stats.applyUsageAt(now, "", "account", "claude-sonnet-test", "", "", responseUsage{InputTokens: 1_000})
		server := &server{pool: &Pool{}, stats: stats}

		want := "\n\n" + lastMonth.Month().String() + ": $1.80\nclaude-opus-test: $1.50\nclaude-sonnet-test: $0.300"
		for _, metric := range server.currentDashboard(now).Overview {
			if metric.Name != "estimated USD this month" {
				continue
			}
			if metric.Value != "$0.0030" || !strings.HasSuffix(metric.Info, want) {
				t.Fatalf("monthly cost = %q with info %q, want last month suffix %q", metric.Value, metric.Info, want)
			}
			return
		}
		t.Fatal("monthly cost overview metric missing")
	})
}

func TestLastMonthCostsLoadFromStoreAndReprice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := openStateStore(t.TempDir() + "/state.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		usage := responseUsage{InputTokens: 1_000, OutputTokens: 100}
		if err := store.recordUsage(storedUsage{At: calendarMonthStart(time.Now()).Add(-time.Hour), Model: "claude-sonnet-test", Usage: usage}); err != nil {
			t.Fatal(err)
		}
		stats, err := newPersistentStats(store, priceSnapshot{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := stats.snapshot().LastMonth; got.APICostNanoDollars != 0 || got.UnpricedResponses != 1 {
			t.Fatalf("last month before refresh = %+v, want one unpriced response", got)
		}

		if err := stats.reprice(lastMonthTestPrices(t)); err != nil {
			t.Fatal(err)
		}
		want := int64(4_500_000)
		snapshot := stats.snapshot()
		if got := snapshot.LastMonth; got.Month != calendarMonthStart(time.Now()).AddDate(0, -1, 0).Month() || got.APICostNanoDollars != want || got.UnpricedResponses != 0 || len(got.ModelCosts) != 1 || got.ModelCosts[0] != (ModelCostSnapshot{Model: "claude-sonnet-test", APICostNanoDollars: want}) {
			t.Fatalf("last month after refresh = %+v, want claude-sonnet-test cost %d", got, want)
		}
		if snapshot.APICostNanoDollars != 0 || snapshot.UnpricedResponses != 0 || len(snapshot.ModelCosts) != 0 {
			t.Fatalf("current month = %d with %d unpriced and models %+v, want empty", snapshot.APICostNanoDollars, snapshot.UnpricedResponses, snapshot.ModelCosts)
		}
	})
}

func TestMonthRolloverKeepsOnlyThePreviousMonth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nextMonth := func() { time.Sleep(time.Until(calendarMonthStart(time.Now()).AddDate(0, 1, 0))) }
		stats := newStatsWithPrices(lastMonthTestPrices(t))
		first := time.Now().Month()
		stats.applyUsageAt(time.Now(), "", "account", "claude-sonnet-test", "", "", responseUsage{InputTokens: 1_000})
		nextMonth()
		second := time.Now().Month()
		stats.applyUsageAt(time.Now(), "", "account", "claude-opus-test", "", "", responseUsage{InputTokens: 1_000})

		snapshot := stats.snapshot()
		if got := snapshot.LastMonth; got.Month != first || len(got.ModelCosts) != 1 || got.ModelCosts[0] != (ModelCostSnapshot{Model: "claude-sonnet-test", APICostNanoDollars: 3_000_000}) {
			t.Fatalf("last month after usage rollover = %+v, want %s claude-sonnet-test cost", got, first)
		}
		if snapshot.APICostNanoDollars != 15_000_000 || len(snapshot.ModelCosts) != 1 {
			t.Fatalf("current month = %d with models %+v, want claude-opus-test cost", snapshot.APICostNanoDollars, snapshot.ModelCosts)
		}

		nextMonth()
		snapshot = stats.snapshot()
		if got := snapshot.LastMonth; got.Month != second || got.APICostNanoDollars != 15_000_000 || snapshot.APICostNanoDollars != 0 {
			t.Fatalf("after an idle month: last month = %+v, current = %d, want %s cost moved to last month", got, snapshot.APICostNanoDollars, second)
		}

		nextMonth()
		nextMonth()
		if got := stats.snapshot().LastMonth; got.APICostNanoDollars != 0 || len(got.ModelCosts) != 0 {
			t.Fatalf("last month after two idle months = %+v, want empty", got)
		}
	})
}
