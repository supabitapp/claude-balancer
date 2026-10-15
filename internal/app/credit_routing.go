package app

import (
	"math"
	"strings"
	"time"
)

func isFableModel(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "fable")
}

func (c routingCandidate) quotaExhausted(model string, now time.Time) bool {
	if c.primary.exhausted(now) || c.secondary.exhausted(now) {
		return true
	}
	return isFableModel(model) && c.fable.exhausted(now)
}

func (c routingCandidate) quotaRetryAt(model string, now time.Time) time.Time {
	var retryAt time.Time
	for _, candidate := range []window{c.primary, c.secondary} {
		if !candidate.exhausted(now) || candidate.resetsAt.IsZero() {
			continue
		}
		if retryAt.IsZero() || candidate.resetsAt.After(retryAt) {
			retryAt = candidate.resetsAt
		}
	}
	if isFableModel(model) && c.fable.exhausted(now) && !c.fable.resetsAt.IsZero() && (retryAt.IsZero() || c.fable.resetsAt.After(retryAt)) {
		retryAt = c.fable.resetsAt
	}
	return retryAt
}

func (c routingCandidate) weeklyExhausted() bool {
	weekly := longestWindow(c.primary, c.secondary)
	return weekly.known() && weekly.minutes >= 7*24*60 && weekly.usedPercent >= 100 && !math.IsInf(weekly.usedPercent, 0)
}

func (c routingCandidate) creditAvailable(now time.Time) bool {
	return c.routingEnabled() && !c.paused && c.reauth == "" && now.After(c.cooldown) &&
		!spendLimitReached(c.spendControl) && !c.creditsRejected && c.weeklyExhausted() && c.credits.spendable()
}

// A usage rejection can mean the upstream cannot spend this account's credits.
// Wait for a fresh usage snapshot before trying its credit balance again.
func (a *Account) rejectCredits() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.creditsRejected = true
}
