package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	claudeFastBeta          = "fast-mode-2026-02-01"
	claudeFastRouteSuffix   = ":fast"
	claudeFastBlockDefault  = time.Hour
	claudeFastMinRetryAfter = 20 * time.Second
	claudeFastMaxRetryAfter = time.Hour
)

type extraUsagePayload struct {
	IsEnabled         *bool `json:"is_enabled"`
	SpendLimitReached bool  `json:"spend_limit_reached"`
}

func claudeFastRequested(speed string) bool {
	speed = strings.ToLower(strings.TrimSpace(speed))
	return speed != "" && speed != "standard"
}

func (a *Account) adoptExtraUsage(extra *extraUsagePayload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if extra == nil {
		a.extraUsage = nil
		return
	}
	cloned := *extra
	a.extraUsage = &cloned
}

func (a *Account) fastEligible(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Before(a.fastBlockedUntil) {
		return false
	}
	if a.extraUsage == nil {
		return true
	}
	enabled := a.extraUsage.IsEnabled == nil || *a.extraUsage.IsEnabled
	return enabled && !a.extraUsage.SpendLimitReached
}

func (a *Account) blockFast(until time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if until.After(a.fastBlockedUntil) {
		a.fastBlockedUntil = until
	}
}

func (a *Account) fastBlockedAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fastBlockedUntil
}

func claudeFastRejection(status int, headers http.Header, body []byte) bool {
	switch status {
	case http.StatusTooManyRequests:
		return claude429ScopeFor(headers, body) != claude429Credential
	case http.StatusBadRequest:
		return bytes.Contains(bytes.ToLower(body), []byte("fast mode is not enabled"))
	default:
		return false
	}
}

func claudeFastBlockUntil(headers http.Header, now time.Time) time.Time {
	if seconds, err := strconv.Atoi(strings.TrimSpace(headers.Get("Retry-After"))); err == nil && seconds > 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	return now.Add(claudeFastBlockDefault)
}

func (s *server) blockClaudeFast(account *Account, model string, response *http.Response) {
	until := claudeFastBlockUntil(response.Header, time.Now())
	account.blockFast(until)
	reason := response.Header.Get("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason")
	s.log.Info("fast mode blocked for account",
		"account", claudeAccountID(account),
		"model", model,
		"upstream_status", response.StatusCode,
		"overage_disabled_reason", reason,
		"until", until,
	)
	if s.stats != nil {
		s.stats.note("fast mode blocked", claudeAccountID(account), strings.TrimSpace(strconv.Itoa(response.StatusCode)+" "+reason))
	}
}

func (s *server) claudeFastRetryAfter(now time.Time) time.Duration {
	var earliest time.Time
	for _, account := range s.pool.all() {
		until := account.fastBlockedAt()
		if until.After(now) && (earliest.IsZero() || until.Before(earliest)) {
			earliest = until
		}
	}
	wait := claudeFastBlockDefault
	if !earliest.IsZero() {
		wait = earliest.Sub(now)
	}
	return min(max(wait, claudeFastMinRetryAfter), claudeFastMaxRetryAfter)
}

// Claude Code treats a fast-mode 429 that has a long Retry-After and no
// overage headers as a temporary capacity limit: it retries the turn at
// standard speed and offers fast mode again after its cooldown.
func writeClaudeFastUnavailable(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.FormatInt(int64((retryAfter+time.Second-1)/time.Second), 10))
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "rate_limit_error",
			"message": "No account in the balancer pool can serve fast mode right now.",
		},
	})
}

func claudeStandardSpeed(body []byte, header http.Header) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) == nil {
		if _, ok := payload["speed"]; ok {
			delete(payload, "speed")
			if stripped, err := json.Marshal(payload); err == nil {
				body = stripped
			}
		}
	}
	values := header.Values("Anthropic-Beta")
	if len(values) == 0 {
		return body
	}
	var kept []string
	for _, value := range values {
		for _, beta := range strings.Split(value, ",") {
			beta = strings.TrimSpace(beta)
			if beta != "" && !strings.EqualFold(beta, claudeFastBeta) {
				kept = append(kept, beta)
			}
		}
	}
	header.Del("Anthropic-Beta")
	if len(kept) > 0 {
		header.Set("Anthropic-Beta", strings.Join(kept, ","))
	}
	return body
}
