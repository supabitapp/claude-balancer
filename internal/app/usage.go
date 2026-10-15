package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var accountAPIBaseURL = "https://api.anthropic.com/api/oauth"

const (
	urgentUsageRefreshInterval   = 2 * time.Minute
	accountDetailRefreshInterval = time.Hour
)

type usagePayload struct {
	PlanType      string             `json:"plan_type"`
	FiveHour      claudeUsageWindow  `json:"five_hour"`
	SevenDay      claudeUsageWindow  `json:"seven_day"`
	SevenDayFable claudeUsageWindow  `json:"seven_day_fable"`
	Limits        []claudeUsageLimit `json:"limits"`
	RateLimit     struct {
		LimitReached    *bool       `json:"limit_reached"`
		PrimaryWindow   usageWindow `json:"primary_window"`
		SecondaryWindow usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	RateLimitResetCredits *struct {
		AvailableCount int64 `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
	SpendControl *spendControlPayload `json:"spend_control"`
	Credits      *creditsPayload      `json:"credits"`
}

type claudeUsageWindow struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type claudeUsageLimit struct {
	Kind     string     `json:"kind"`
	Percent  *float64   `json:"percent"`
	ResetsAt *time.Time `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

func (u claudeUsageWindow) window(fetchedAt time.Time, minutes int) window {
	if u.Utilization == nil {
		return window{}
	}
	result := window{
		usedPercent: *u.Utilization,
		minutes:     minutes,
		seenAt:      fetchedAt,
	}
	if u.ResetsAt != nil {
		result.resetsAt = *u.ResetsAt
	}
	return result
}

type creditsPayload struct {
	HasCredits          bool   `json:"has_credits"`
	Unlimited           bool   `json:"unlimited"`
	OverageLimitReached bool   `json:"overage_limit_reached"`
	Balance             string `json:"balance"`
}

func (c *creditsPayload) spendable() bool {
	if c == nil || c.OverageLimitReached {
		return false
	}
	if c.Unlimited {
		return true
	}
	if !c.HasCredits {
		return false
	}
	balance, err := strconv.ParseFloat(strings.TrimSpace(c.Balance), 64)
	return err == nil && balance > 0 && !math.IsInf(balance, 0)
}

func cloneCredits(value *creditsPayload) *creditsPayload {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type spendControlPayload struct {
	Reached         bool               `json:"reached"`
	IndividualLimit *spendControlLimit `json:"individual_limit"`
}

type spendControlLimit struct {
	Source            string   `json:"source"`
	Limit             string   `json:"limit"`
	Used              string   `json:"used"`
	Remaining         string   `json:"remaining"`
	UsedPercent       *float64 `json:"used_percent"`
	RemainingPercent  *float64 `json:"remaining_percent"`
	ResetAfterSeconds int64    `json:"reset_after_seconds"`
	ResetAt           int64    `json:"reset_at"`
}

func spendLimitReached(control *spendControlPayload) bool {
	return control != nil && control.Reached
}

func cloneSpendControl(value *spendControlPayload) *spendControlPayload {
	if value == nil {
		return nil
	}
	cloned := *value
	if value.IndividualLimit != nil {
		limit := *value.IndividualLimit
		if value.IndividualLimit.UsedPercent != nil {
			used := *value.IndividualLimit.UsedPercent
			limit.UsedPercent = &used
		}
		if value.IndividualLimit.RemainingPercent != nil {
			remaining := *value.IndividualLimit.RemainingPercent
			limit.RemainingPercent = &remaining
		}
		cloned.IndividualLimit = &limit
	}
	return &cloned
}

func (p usagePayload) bankedResets() *int64 {
	if p.RateLimitResetCredits == nil {
		return nil
	}
	return &p.RateLimitResetCredits.AvailableCount
}

func (p usagePayload) windows(fetchedAt time.Time) (primary, secondary, fable window) {
	primary = p.RateLimit.PrimaryWindow.window(fetchedAt)
	secondary = p.RateLimit.SecondaryWindow.window(fetchedAt)
	fable = p.SevenDayFable.window(fetchedAt, 7*24*60)
	if direct := p.FiveHour.window(fetchedAt, 5*60); direct.known() {
		primary = direct
	}
	if direct := p.SevenDay.window(fetchedAt, 7*24*60); direct.known() {
		secondary = direct
	}
	for _, limit := range p.Limits {
		minutes := 0
		switch limit.Kind {
		case "session":
			minutes = 5 * 60
		case "weekly_all", "weekly_scoped":
			minutes = 7 * 24 * 60
		default:
			continue
		}
		if limit.Percent == nil {
			continue
		}
		fallback := claudeUsageWindow{Utilization: limit.Percent, ResetsAt: limit.ResetsAt}.window(fetchedAt, minutes)
		if limit.Kind == "session" && !primary.known() {
			primary = fallback
		}
		if limit.Kind == "weekly_all" && !secondary.known() {
			secondary = fallback
		}
		if limit.Kind == "weekly_scoped" && isFableLimit(limit) && !fable.known() {
			fable = fallback
		}
	}
	return primary, secondary, fable
}

func isFableLimit(limit claudeUsageLimit) bool {
	return limit.Scope != nil && limit.Scope.Model != nil && strings.EqualFold(strings.TrimSpace(limit.Scope.Model.DisplayName), "fable")
}

type usageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	ResetAt            int64    `json:"reset_at"`
	LimitWindowSeconds int      `json:"limit_window_seconds"`
}

type resetCreditsPayload struct {
	Credits        []resetCredit `json:"credits"`
	AvailableCount int64         `json:"available_count"`
}

type resetCredit struct {
	ID          string     `json:"id"`
	ResetType   string     `json:"reset_type"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
}

func (c resetCredit) available() bool {
	return c.ResetType == "claude_rate_limits" && c.Status == "available"
}

type consumeResetCreditRequest struct {
	RedeemRequestID string `json:"redeem_request_id"`
	CreditID        string `json:"credit_id"`
}

type consumeResetCreditResponse struct {
	Code         string `json:"code"`
	WindowsReset int64  `json:"windows_reset"`
}

func (u usageWindow) window(fetchedAt time.Time) window {
	if u.UsedPercent == nil {
		return window{}
	}
	w := window{
		usedPercent: *u.UsedPercent,
		minutes:     u.LimitWindowSeconds / 60,
		seenAt:      fetchedAt,
	}
	if u.ResetAt > 0 {
		w.resetsAt = time.Unix(u.ResetAt, 0)
	}
	return w
}

func (s *server) pollUsage(ctx context.Context, account *Account) error {
	resp, err := s.doAccountRequest(ctx, account, http.MethodGet, accountAPIBaseURL+"/usage", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("usage returned %s", resp.Status)
	}

	var payload usagePayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	fetchedAt := time.Now()
	primary, secondary, fable := payload.windows(fetchedAt)
	account.adopt(
		fetchedAt,
		payload.PlanType,
		primary,
		secondary,
		fable,
		payload.bankedResets(),
		payload.SpendControl,
		payload.Credits,
	)
	if err := s.persistClaudePlan(ctx, account, payload.PlanType); err != nil && ctx.Err() == nil {
		s.log.Debug("plan metadata persistence failed", "account", account.id(), "error", err)
	}
	if err := s.pollClaudePlan(ctx, account); err != nil && ctx.Err() == nil {
		s.log.Debug("plan metadata poll failed", "account", account.id(), "error", err)
	}
	var limitReached any
	if payload.RateLimit.LimitReached != nil {
		limitReached = *payload.RateLimit.LimitReached
		if *payload.RateLimit.LimitReached && account.markSpent() {
			s.log.Info("account stopped accepting new websockets",
				"account", account.id(),
				"source", "usage_poll",
			)
		}
	}
	attrs := []any{"reported_limit_reached", limitReached}
	attrs = append(attrs, routingLogAttrs(account.routingCandidate(), time.Now())...)
	s.log.Debug("usage polled", attrs...)
	return nil
}

func (s *server) pollClaudePlan(ctx context.Context, account *Account) error {
	if !strings.EqualFold(account.claims().Provider, "claude") || !account.planMetadataDue(time.Now()) {
		return nil
	}
	resp, err := s.doAccountRequest(ctx, account, http.MethodGet, claudeOAuthProfileURL, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("profile returned %s", resp.Status)
	}
	var profile claudeOAuthProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return err
	}
	account.markPlanMetadataFetched(time.Now())
	plan := claudeProfilePlan(&profile)
	if plan == "" {
		return nil
	}
	if err := s.persistClaudePlan(ctx, account, plan); err != nil {
		return err
	}
	if s.pool != nil {
		for _, current := range s.pool.all() {
			if current.id() == account.id() {
				current.markPlanMetadataFetched(time.Now())
				break
			}
		}
	}
	return nil
}

func (s *server) persistClaudePlan(ctx context.Context, account *Account, plan string) error {
	if !account.setPlanType(plan) {
		return nil
	}
	if s.pool == nil {
		return nil
	}
	if _, err := s.pool.persistAccountStateContext(ctx, account.persisted()); err != nil {
		return fmt.Errorf("persist plan metadata: %w", err)
	}
	return nil
}

func (s *server) pollResetCredits(ctx context.Context, account *Account) error {
	resp, err := s.doAccountRequest(ctx, account, http.MethodGet, accountAPIBaseURL+"/rate-limit-reset-credits", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		account.markResetCreditsUnavailable(time.Now())
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("reset credits returned %s", resp.Status)
	}
	var payload resetCreditsPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	fetchedAt := time.Now()
	account.adoptResetCredits(fetchedAt, payload.AvailableCount, payload.Credits)
	return nil
}

func (s *server) doAccountRequest(ctx context.Context, account *Account, method, endpoint string, body []byte) (*http.Response, error) {
	canReauth := true
	if account.refreshDue(time.Now()) {
		if err := s.reauthorize(account); err != nil {
			return nil, err
		}
		canReauth = false
	}
	for ; ; canReauth = false {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		account.mu.Lock()
		token := account.AccessToken
		account.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("anthropic-account-id", account.id())
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || !canReauth {
			return resp, nil
		}
		resp.Body.Close()
		if err := s.reauthorize(account); err != nil {
			return nil, err
		}
	}
}

// A zero lead allows any unexpired credit, with undated credits ordered last.
func nextResetCredit(credits []resetCredit, now time.Time, lead time.Duration) (resetCredit, bool) {
	deadline := now.Add(lead)
	var next resetCredit
	found := false
	for _, credit := range credits {
		if !credit.available() {
			continue
		}
		if credit.ExpiresAt != nil && !credit.ExpiresAt.After(now) {
			continue
		}
		if lead > 0 && (credit.ExpiresAt == nil || credit.ExpiresAt.After(deadline)) {
			continue
		}
		if !found || resetCreditExpiresBefore(credit, next) {
			next = credit
			found = true
		}
	}
	return next, found
}

func resetCreditExpiresBefore(credit, other resetCredit) bool {
	return credit.ExpiresAt != nil && (other.ExpiresAt == nil || credit.ExpiresAt.Before(*other.ExpiresAt))
}

func expiringResetCredit(credits []resetCredit, now time.Time) (resetCredit, bool) {
	return nextResetCredit(credits, now, resetPriorityLead)
}

func (s *server) consumeResetCredit(ctx context.Context, account *Account, choose func([]resetCredit) (resetCredit, bool)) (consumeResetCreditResponse, string, error) {
	resp, err := s.doAccountRequest(ctx, account, http.MethodGet, accountAPIBaseURL+"/rate-limit-reset-credits", nil)
	if err != nil {
		return consumeResetCreditResponse{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return consumeResetCreditResponse{}, "", fmt.Errorf("reset credits returned %s", resp.Status)
	}

	var payload resetCreditsPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return consumeResetCreditResponse{}, "", err
	}
	fetchedAt := time.Now()
	account.adoptResetCredits(fetchedAt, payload.AvailableCount, payload.Credits)
	credit, ok := choose(payload.Credits)
	if !ok {
		return consumeResetCreditResponse{}, "", nil
	}

	body, err := json.Marshal(consumeResetCreditRequest{
		RedeemRequestID: credit.ID,
		CreditID:        credit.ID,
	})
	if err != nil {
		return consumeResetCreditResponse{}, credit.ID, err
	}
	resp, err = s.doAccountRequest(ctx, account, http.MethodPost, accountAPIBaseURL+"/rate-limit-reset-credits/consume", body)
	if err != nil {
		return consumeResetCreditResponse{}, credit.ID, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return consumeResetCreditResponse{}, credit.ID, fmt.Errorf("reset credit consume returned %s", resp.Status)
	}

	var result consumeResetCreditResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return consumeResetCreditResponse{}, credit.ID, err
	}
	switch result.Code {
	case "reset", "nothing_to_reset", "no_credit", "already_redeemed":
		return result, credit.ID, nil
	default:
		return consumeResetCreditResponse{}, credit.ID, fmt.Errorf("reset credit consume returned code %q", result.Code)
	}
}

func (s *server) reauthorize(account *Account) error {
	if !s.refreshed(account, account.id()) {
		return fmt.Errorf("account %s needs reauth", account.id())
	}
	return nil
}

func (a *Account) adopt(fetchedAt time.Time, planType string, primary, secondary, fable window, banked *int64, spendControl *spendControlPayload, credits *creditsPayload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageFetchedAt = fetchedAt
	a.spendControl = cloneSpendControl(spendControl)
	a.credits = cloneCredits(credits)
	a.creditsRejected = false
	if planType != "" {
		a.planType = planType
	}
	if primary.known() {
		a.primary = primary
	}
	if secondary.known() {
		a.secondary = secondary
	}
	if fable.known() {
		a.fable = fable
	}
	if banked != nil && (!a.resetCredits.known || a.resetCredits.count != *banked) {
		a.resetCredits = resetCreditState{known: true, count: *banked}
	}
	if a.primary.exhausted(fetchedAt) || a.secondary.exhausted(fetchedAt) || spendLimitReached(a.spendControl) {
		a.spent = true
	} else if (a.primary.known() || a.secondary.known()) && a.pressure() < 100 {
		a.spent = false
	}
}

func (a *Account) adoptResetCredits(fetchedAt time.Time, count int64, credits []resetCredit) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resetCredits = resetCreditState{
		fetchedAt: fetchedAt,
		known:     true,
		count:     count,
		details:   append([]resetCredit(nil), credits...),
	}
}

func (a *Account) markResetCreditsUnavailable(fetchedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resetCredits = resetCreditState{fetchedAt: fetchedAt}
}

func (a *Account) pollsDue(now time.Time, every time.Duration) (usage, resetCredits bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	usageEvery := every
	if a.spent || spendLimitReached(a.spendControl) || now.Before(a.cooldown) || a.pressure() >= 95 {
		usageEvery = min(usageEvery, urgentUsageRefreshInterval)
	}
	usage = a.usageFetchedAt.IsZero() || now.Sub(a.usageFetchedAt) >= usageEvery
	resetCredits = a.resetCredits.fetchedAt.IsZero() || now.Sub(a.resetCredits.fetchedAt) >= accountDetailRefreshInterval
	return usage, resetCredits
}

func (s *server) pollAllUsage(ctx context.Context) {
	s.pollAccountData(ctx, 0, true)
}

func (s *server) pollDueUsage(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	s.pollAccountData(ctx, every, false)
}

func (s *server) pollAccountData(ctx context.Context, every time.Duration, force bool) {
	for _, account := range s.pool.all() {
		if account.needsReauth() {
			continue
		}
		poll := func(due bool, kind string, run func() error) bool {
			if due {
				if err := run(); err != nil && ctx.Err() == nil {
					message := kind + " poll failed"
					s.log.Warn(message, "account", account.id(), "error", err)
					s.stats.note(message, account.id(), err.Error())
				}
			}
			return ctx.Err() == nil
		}
		usageDue, resetCreditsDue := true, true
		if !force {
			usageDue, resetCreditsDue = account.pollsDue(time.Now(), every)
		}
		if !poll(usageDue, "usage", func() error { return s.pollUsage(ctx, account) }) {
			return
		}
		if account.needsReauth() {
			continue
		}
		if !force {
			_, resetCreditsDue = account.pollsDue(time.Now(), every)
		}
		if !poll(resetCreditsDue, "reset credits", func() error {
			return s.pollResetCredits(ctx, account)
		}) {
			return
		}
	}
}

func (s *server) watchUsage(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}

	ticker := time.NewTicker(min(every, urgentUsageRefreshInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollDueUsage(ctx, every)
		}
	}
}
