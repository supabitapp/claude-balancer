package app

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestClaudeRateLimitResetUsesLimitedWindows(t *testing.T) {
	now := time.Date(2026, time.October, 16, 20, 49, 0, 0, time.UTC)
	fiveHourReset := now.Add(90 * time.Minute)
	sevenDayReset := now.Add(6 * 24 * time.Hour)
	for _, test := range []struct {
		name      string
		fiveHour  string
		sevenDay  string
		fiveUsed  string
		sevenUsed string
		want      time.Time
	}{
		{name: "five hour rejected", fiveHour: "rejected", sevenDay: "allowed", want: fiveHourReset},
		{name: "seven day rejected", fiveHour: "allowed", sevenDay: "rejected", want: sevenDayReset},
		{name: "both rejected", fiveHour: "rejected", sevenDay: "rejected", want: sevenDayReset},
		{name: "five hour exhausted", fiveUsed: "1", sevenUsed: "0.23", want: fiveHourReset},
		{name: "seven day exhausted", fiveUsed: "0.23", sevenUsed: "1", want: sevenDayReset},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{
				"anthropic-ratelimit-unified-status":         {"rejected"},
				"anthropic-ratelimit-unified-reset":          {strconv.FormatInt(sevenDayReset.Unix(), 10)},
				"anthropic-ratelimit-unified-5h-status":      {test.fiveHour},
				"anthropic-ratelimit-unified-7d-status":      {test.sevenDay},
				"anthropic-ratelimit-unified-5h-utilization": {test.fiveUsed},
				"anthropic-ratelimit-unified-7d-utilization": {test.sevenUsed},
				"anthropic-ratelimit-unified-5h-reset":       {strconv.FormatInt(fiveHourReset.Unix(), 10)},
				"anthropic-ratelimit-unified-7d-reset":       {strconv.FormatInt(sevenDayReset.Unix(), 10)},
			}
			if got := claudeRateLimitReset(headers, now); !got.Equal(test.want) {
				t.Fatalf("reset = %s, want %s", got, test.want)
			}
		})
	}
}

func TestClaudeRateLimitResetFallbacks(t *testing.T) {
	now := time.Date(2026, time.October, 16, 20, 49, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		headers http.Header
		want    time.Time
	}{
		{
			name: "unified reset without window signals",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-Status": {"rejected"},
				"Anthropic-Ratelimit-Unified-Reset":  {now.Add(time.Hour).Format(http.TimeFormat)},
			},
			want: now.Add(time.Hour),
		},
		{
			name: "retry after without window reset",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
				"Retry-After":                           {"60"},
			},
			want: now.Add(time.Minute),
		},
		{
			name: "retry after exceeds window reset",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status":   {"rejected"},
				"Anthropic-Ratelimit-Unified-5h-Reset-At": {now.Add(time.Minute).Format(http.TimeFormat)},
				"Retry-After": {now.Add(2 * time.Minute).Format(http.TimeFormat)},
			},
			want: now.Add(2 * time.Minute),
		},
		{
			name: "healthy weekly reset without limited window reset",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
				"Anthropic-Ratelimit-Unified-7d-Status": {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Reset":  {now.Add(6 * 24 * time.Hour).Format(http.TimeFormat)},
			},
		},
		{
			name: "unreasonable reset",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"},
				"Anthropic-Ratelimit-Unified-7d-Reset":  {now.Add(30 * 24 * time.Hour).Format(http.TimeFormat)},
			},
			want: now.Add(minCooldown),
		},
		{
			name: "model rejection",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed"},
				"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
				"Retry-After": {"60"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := claudeRateLimitReset(test.headers, now); !got.Equal(test.want) {
				t.Fatalf("reset = %s, want %s", got, test.want)
			}
		})
	}
}

func TestClaudeFiveHourCooldownRecoversBeforeWeeklyReset(t *testing.T) {
	now := time.Now()
	fiveHourReset := now.Add(90 * time.Minute).Truncate(time.Second)
	sevenDayReset := now.Add(6 * 24 * time.Hour).Truncate(time.Second)
	account := testAccount("account-a", 23)
	account.observeClaudeHeaders(http.Header{
		"anthropic-ratelimit-unified-status":    {"rejected"},
		"anthropic-ratelimit-unified-5h-status": {"rejected"},
		"anthropic-ratelimit-unified-7d-status": {"allowed"},
		"anthropic-ratelimit-unified-5h-reset":  {strconv.FormatInt(fiveHourReset.Unix(), 10)},
		"anthropic-ratelimit-unified-7d-reset":  {strconv.FormatInt(sevenDayReset.Unix(), 10)},
	})
	if got := account.status(now); got != accountCooling {
		t.Fatalf("status before reset = %s, want cooling", got)
	}
	if got := account.status(fiveHourReset.Add(time.Second)); got != accountLive {
		t.Fatalf("status after five-hour reset = %s, want live", got)
	}
}
