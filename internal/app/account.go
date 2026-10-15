package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	tokenRefreshFallback = 8 * 24 * time.Hour
	tokenRefreshLead     = 5 * time.Minute
)

var oauthEndpoint = claudeOAuthTokenURL

type accountState struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	Paused       bool
	RoutingMode  routingMode
	LastRefresh  time.Time
	Reauth       string
}

type Account struct {
	accountState
	mu                  sync.Mutex
	resetMu             sync.Mutex
	inflight            *accountRefresh
	rejectedAccessToken authorizationRevision

	cooldown        time.Time
	modelCooldown   map[string]time.Time
	planType        string
	primary         window
	secondary       window
	fable           window
	spent           bool
	resetCredits    resetCreditState
	spendControl    *spendControlPayload
	credits         *creditsPayload
	creditsRejected bool
	usageFetchedAt  time.Time
	planFetchedAt   time.Time
	lastUsed        time.Time
}

type authorizationRevision [sha256.Size]byte

type resetCreditState struct {
	fetchedAt time.Time
	known     bool
	count     int64
	details   []resetCredit
}

type window struct {
	usedPercent float64
	minutes     int
	resetsAt    time.Time
	seenAt      time.Time
}

type accountStatus string

type routingMode string

const (
	accountLive        accountStatus = "live"
	accountPriority    accountStatus = "priority"
	accountCredits     accountStatus = "credits"
	accountChecking    accountStatus = "checking"
	accountCooling     accountStatus = "cooling"
	accountPaused      accountStatus = "paused"
	accountNeedsReauth accountStatus = "needs_reauth"
	accountNotRouted   accountStatus = "not_routed"

	routingModeNormal   routingMode = "normal"
	routingModePriority routingMode = "priority"
	routingModePaused   routingMode = "paused"
)

func (m routingMode) normalized() routingMode {
	switch m {
	case routingModePriority:
		return m
	default:
		return routingModeNormal
	}
}

func (m routingMode) next() routingMode {
	switch m {
	case routingModePriority:
		return routingModePaused
	case routingModePaused:
		return routingModeNormal
	default:
		return routingModePriority
	}
}

func (s accountState) effectiveRoutingMode() routingMode {
	if s.Paused {
		return routingModePaused
	}
	return s.RoutingMode.normalized()
}

func (w window) known() bool { return !w.seenAt.IsZero() }

func (w window) exhausted(now time.Time) bool {
	return w.known() && w.usedPercent >= 100 && (w.resetsAt.IsZero() || w.resetsAt.After(now))
}

func remainingPercent(w window) (float64, bool) {
	if !w.known() {
		return 0, false
	}
	return min(max(100-w.usedPercent, 0), 100), true
}

func longestWindow(windows ...window) window {
	var longest window
	for _, w := range windows {
		if w.known() && (!longest.known() || w.minutes > longest.minutes) {
			longest = w
		}
	}
	return longest
}

func creditCycleStart(now time.Time, windows ...window) (time.Time, bool) {
	cycle := longestWindow(windows...)
	duration := time.Duration(cycle.minutes) * time.Minute
	if !cycle.known() || duration <= 0 || !cycle.resetsAt.After(now) {
		return time.Time{}, false
	}
	start := cycle.resetsAt.Add(-duration)
	if start.After(now) {
		return time.Time{}, false
	}
	return start, true
}

func routablePlan(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "business", "enterprise":
		return false
	default:
		return true
	}
}

func (a *Account) pressure() float64 {
	return math.Max(a.primary.usedPercent, a.secondary.usedPercent)
}

func (a *Account) health() (primary, secondary window, cooldown time.Time, reauth string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.primary, a.secondary, a.cooldown, a.Reauth
}

func (a *Account) markSpent() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := !a.spent
	a.spent = true
	return changed
}

func (a *Account) restoreFromUsageAfter(t time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pressure() >= 100 || spendLimitReached(a.spendControl) {
		return false
	}
	known := false
	for _, window := range []window{a.primary, a.secondary} {
		if !window.known() {
			continue
		}
		known = true
		if !window.seenAt.After(t) {
			return false
		}
	}
	if known {
		a.spent = false
	}
	return known
}

func (a *Account) status(now time.Time) accountStatus {
	return a.routingCandidate().status(now)
}

func accountStatusAt(paused bool, reauth string, cooldown time.Time, spent, known bool, now time.Time) accountStatus {
	switch {
	case paused:
		return accountPaused
	case reauth != "":
		return accountNeedsReauth
	case spent || now.Before(cooldown):
		return accountCooling
	case !known:
		return accountChecking
	default:
		return accountLive
	}
}

func (a *Account) bankedResets() (int64, []resetCredit, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.resetCredits.known {
		return 0, nil, false
	}
	return a.resetCredits.count, append([]resetCredit(nil), a.resetCredits.details...), true
}

func (a *Account) persisted() accountState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accountState
}

func accountFromState(state accountState) *Account {
	state.RoutingMode = state.RoutingMode.normalized()
	return &Account{accountState: state}
}

func (a *Account) applyPersisted(next accountState) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	before := a.accountState
	if !next.LastRefresh.Before(a.LastRefresh) {
		credentialsChanged := a.IDToken != next.IDToken ||
			a.AccessToken != next.AccessToken ||
			a.RefreshToken != next.RefreshToken
		a.IDToken = next.IDToken
		accessTokenChanged := a.AccessToken != next.AccessToken
		a.AccessToken = next.AccessToken
		a.RefreshToken = next.RefreshToken
		a.LastRefresh = next.LastRefresh
		if credentialsChanged {
			next.Reauth = ""
		}
		if accessTokenChanged {
			a.rejectedAccessToken = authorizationRevision{}
		}
		a.Reauth = next.Reauth
	}
	a.Paused = next.Paused
	a.RoutingMode = next.RoutingMode.normalized()
	return a.accountState != before
}

type authClaims struct {
	Provider         string `json:"provider"`
	Email            string `json:"email"`
	PlanType         string `json:"plan_type"`
	AccountID        string `json:"account_id"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	DeviceID         string `json:"device_id"`
	Auth             struct {
		AccountID               string `json:"account_id"`
		Plan                    string `json:"plan"`
		SubscriptionActiveUntil string `json:"subscription_active_until"`
		SubscriptionLastChecked string `json:"subscription_last_checked"`
	} `json:"auth"`
}

func jwtClaims(token string, into any) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	json.Unmarshal(raw, into)
}

func (a *Account) claims() authClaims {
	a.mu.Lock()
	defer a.mu.Unlock()
	return claimsFromToken(a.IDToken)
}

func claimsFromToken(token string) authClaims {
	var c authClaims
	jwtClaims(token, &c)
	if c.AccountID == "" && c.Auth.AccountID == "" {
		_ = json.Unmarshal([]byte(token), &c)
	}
	if c.Auth.AccountID == "" {
		c.Auth.AccountID = c.AccountID
	}
	return c
}

func tokenExpiry(token string) time.Time {
	var c struct {
		Exp int64 `json:"exp"`
	}
	jwtClaims(token, &c)
	if c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

func (a *Account) expires() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if expiry := tokenExpiry(a.AccessToken); !expiry.IsZero() {
		return expiry
	}
	if !strings.EqualFold(claimsFromToken(a.IDToken).Provider, "claude") {
		return time.Time{}
	}
	var identity struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if json.Unmarshal([]byte(a.IDToken), &identity) != nil {
		return time.Time{}
	}
	return identity.ExpiresAt
}

func (a *Account) markRejectedAccessToken(token authorizationRevision) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if accessTokenDigest(a.AccessToken) != token || a.rejectedAccessToken == token {
		return false
	}
	a.rejectedAccessToken = token
	return true
}

func (a *Account) clearRejectedAccessToken(token authorizationRevision) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rejectedAccessToken == token {
		a.rejectedAccessToken = authorizationRevision{}
	}
}

func accessTokenDigest(token string) authorizationRevision {
	return authorizationRevision(sha256.Sum256([]byte(token)))
}

func (a *Account) id() string {
	claims := a.claims()
	if claims.Auth.AccountID != "" {
		return claims.Auth.AccountID
	}
	return claims.AccountID
}

func accountIDFromToken(token string) string {
	claims := claimsFromToken(token)
	if claims.Auth.AccountID != "" {
		return claims.Auth.AccountID
	}
	return claims.AccountID
}

func (a *Account) email() string { return a.claims().Email }

func (a *Account) plan() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.planType != "" {
		return a.planType
	}
	claims := claimsFromToken(a.IDToken)
	if claims.PlanType != "" {
		return claims.PlanType
	}
	return claims.Auth.Plan
}

func (a *Account) setPlanType(plan string) bool {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !strings.EqualFold(claimsFromToken(a.IDToken).Provider, "claude") {
		return false
	}
	credentials := claudeCredentialsFromIdentityToken(a.IDToken)
	if credentials.PlanType == plan {
		a.planType = plan
		return false
	}
	credentials.PlanType = plan
	a.planType = plan
	a.IDToken = claudeIdentityToken(credentials)
	return true
}

func (a *Account) planMetadataDue(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planFetchedAt.IsZero() || now.Sub(a.planFetchedAt) >= accountDetailRefreshInterval
}

func (a *Account) markPlanMetadataFetched(at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planFetchedAt = at
}

func (a *Account) paused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Paused
}

func (a *Account) needsReauth() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Reauth != ""
}

func (a *Account) refreshDue(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if expiry := tokenExpiry(a.AccessToken); !expiry.IsZero() {
		return !expiry.After(now.Add(tokenRefreshLead))
	}
	claims := claimsFromToken(a.IDToken)
	if strings.EqualFold(claims.Provider, "claude") {
		var identity struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		if json.Unmarshal([]byte(a.IDToken), &identity) == nil && !identity.ExpiresAt.IsZero() {
			return !identity.ExpiresAt.After(now.Add(tokenRefreshLead))
		}
	}
	return now.Sub(a.LastRefresh) > tokenRefreshFallback
}

type refreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

var permanentRefreshFailures = []string{
	"refresh_token_expired",
	"refresh_token_reused",
	"refresh_token_invalidated",
	"invalid_grant",
}

func (a *Account) finishRefresh(ctx context.Context, persist func(context.Context, accountState) (accountState, error), state accountState, tokens tokenResponse, permanent bool, err error) (error, error) {
	token := state.RefreshToken
	var completionErr error
	next := state
	if err == nil {
		if tokens.AccessToken != "" {
			next.AccessToken = tokens.AccessToken
		}
		if tokens.RefreshToken != "" {
			next.RefreshToken = tokens.RefreshToken
		}
		if tokens.IDToken != "" {
			next.IDToken = tokens.IDToken
		}
		next.LastRefresh = time.Now()
		next.Reauth = ""
		if persist != nil {
			next, err = persist(ctx, next)
			completionErr = err
		}
	} else if permanent {
		next.Reauth = err.Error()
		if persist != nil {
			if persisted, persistErr := persist(ctx, next); persistErr != nil {
				completionErr = persistErr
				err = errors.Join(err, persistErr)
			} else {
				next = persisted
			}
		}
	}

	a.mu.Lock()
	superseded := a.RefreshToken != token && a.RefreshToken != next.RefreshToken
	if superseded {
		err = nil
		completionErr = nil
		permanent = false
	}
	if !superseded {
		switch {
		case err != nil && permanent:
			a.Reauth = next.Reauth
		case err == nil:
			accessTokenChanged := a.AccessToken != next.AccessToken
			a.accountState = next
			if accessTokenChanged {
				a.rejectedAccessToken = authorizationRevision{}
			}
		}
	}
	a.mu.Unlock()
	return err, completionErr
}

func exchangeRefreshToken(ctx context.Context, hc *http.Client, token string) (tokenResponse, bool, error) {
	var out tokenResponse
	body, err := json.Marshal(refreshRequest{
		ClientID:     claudeOAuthClientID,
		GrantType:    "refresh_token",
		RefreshToken: token,
	})
	if err != nil {
		return out, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthEndpoint, bytes.NewReader(body))
	if err != nil {
		return out, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return out, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		for _, reason := range permanentRefreshFailures {
			if strings.Contains(string(body), reason) {
				return out, true, errors.New(reason)
			}
		}
		return out, resp.StatusCode == http.StatusUnauthorized,
			fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, false, err
	}
	return out, false, nil
}
