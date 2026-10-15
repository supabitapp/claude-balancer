package app

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUsagePayloadWindowsUsesClaudeOAuthWindows(t *testing.T) {
	fetchedAt := time.Date(2026, time.October, 15, 17, 30, 0, 0, time.UTC)
	var payload usagePayload
	if err := json.Unmarshal([]byte(`{
		"five_hour":{"utilization":12.5,"resets_at":"2026-10-15T22:00:00Z"},
		"seven_day":{"utilization":37,"resets_at":"2026-10-22T00:00:00Z"}
	}`), &payload); err != nil {
		t.Fatal(err)
	}

	primary, secondary := payload.windows(fetchedAt)
	if !primary.known() || primary.usedPercent != 12.5 || primary.minutes != 5*60 {
		t.Fatalf("primary = %+v, want 12.5%% over 5 hours", primary)
	}
	if !primary.resetsAt.Equal(time.Date(2026, time.October, 15, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("primary reset = %s", primary.resetsAt)
	}
	if !secondary.known() || secondary.usedPercent != 37 || secondary.minutes != 7*24*60 {
		t.Fatalf("secondary = %+v, want 37%% over 7 days", secondary)
	}
	if !secondary.resetsAt.Equal(time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("secondary reset = %s", secondary.resetsAt)
	}
}

func TestUsagePayloadWindowsFallsBackToLegacyRateLimitWindows(t *testing.T) {
	fetchedAt := time.Date(2026, time.October, 15, 17, 30, 0, 0, time.UTC)
	var payload usagePayload
	if err := json.Unmarshal([]byte(`{
		"rate_limit":{
			"primary_window":{"used_percent":10,"limit_window_seconds":300,"reset_at":1792101600},
			"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1792713600}
		}
	}`), &payload); err != nil {
		t.Fatal(err)
	}

	primary, secondary := payload.windows(fetchedAt)
	if !primary.known() || primary.usedPercent != 10 || primary.minutes != 5 {
		t.Fatalf("primary = %+v, want legacy 10%% over 5 minutes", primary)
	}
	if !secondary.known() || secondary.usedPercent != 20 || secondary.minutes != 10080 {
		t.Fatalf("secondary = %+v, want legacy 20%% over 7 days", secondary)
	}
}

func TestUsagePayloadWindowsFallsBackToClaudeLimits(t *testing.T) {
	fetchedAt := time.Date(2026, time.October, 15, 17, 30, 0, 0, time.UTC)
	var payload usagePayload
	if err := json.Unmarshal([]byte(`{
		"limits":[
			{"kind":"session","percent":3,"resets_at":"2026-10-15T22:00:00Z"},
			{"kind":"weekly_all","percent":46,"resets_at":"2026-10-22T00:00:00Z"},
			{"kind":"weekly_scoped","percent":88,"resets_at":"2026-10-22T00:00:00Z"}
		]
	}`), &payload); err != nil {
		t.Fatal(err)
	}

	primary, secondary := payload.windows(fetchedAt)
	if primary.usedPercent != 3 || primary.minutes != 5*60 || !primary.resetsAt.Equal(time.Date(2026, time.October, 15, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("primary = %+v, want session limit", primary)
	}
	if secondary.usedPercent != 46 || secondary.minutes != 7*24*60 || !secondary.resetsAt.Equal(time.Date(2026, time.October, 22, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("secondary = %+v, want weekly_all limit", secondary)
	}
}
