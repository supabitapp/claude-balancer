package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	claudeMessagesPath       = "/v1/messages"
	claudeMessagesAliasPath  = "/messages"
	claudeMaxRequestBody     = 32 << 20
	claudeMaxResponseBody    = 256 << 20
	claudeUpstreamTimeout    = 90 * time.Second
	claudeMaxFailoverAttempt = 2
)

// claudeMessageRequest contains only the fields needed to choose a route. The
// original JSON is forwarded unchanged to preserve Anthropic's wire format.
type claudeMessageRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Metadata struct {
		UserID    json.RawMessage `json:"user_id"`
		ParentID  string          `json:"parent_id"`
		SessionID string          `json:"session_id"`
	} `json:"metadata"`
}

type claudeUserIdentity struct {
	SessionID       string `json:"session_id"`
	ParentSessionID string `json:"parent_session_id"`
	AgentID         string `json:"agent_id"`
}

func claudeRequestFromBody(body []byte) claudeMessageRequest {
	var request claudeMessageRequest
	_ = json.Unmarshal(body, &request)
	return request
}

func claudeIdentityFromMetadata(raw json.RawMessage) claudeUserIdentity {
	var identity claudeUserIdentity
	if len(raw) == 0 {
		return identity
	}
	if json.Unmarshal(raw, &identity) == nil && (identity.SessionID != "" || identity.AgentID != "" || identity.ParentSessionID != "") {
		return identity
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		_ = json.Unmarshal([]byte(encoded), &identity)
	}
	return identity
}

type claudeRoute struct {
	thread  string
	session string
}

func claudeRouteKey(model, value string) string {
	model = strings.TrimSpace(model)
	value = strings.TrimSpace(value)
	return "claude:" + model + ":" + value
}

func (r claudeRoute) keys() []string {
	keys := make([]string, 0, 2)
	if r.thread != "" {
		keys = append(keys, r.thread)
	}
	if r.session != "" && r.session != r.thread {
		keys = append(keys, r.session)
	}
	return keys
}

func claudeRouteFrom(r *http.Request, body claudeMessageRequest) claudeRoute {
	identity := claudeIdentityFromMetadata(body.Metadata.UserID)
	session := firstWebSocketHeader(r.Header,
		"x-claude-code-session-id", "x-session-id", "session-id", "session_id")
	if session == "" {
		session = strings.TrimSpace(body.Metadata.SessionID)
	}
	if session == "" {
		session = strings.TrimSpace(identity.SessionID)
	}
	thread := firstWebSocketHeader(r.Header,
		"x-claude-code-agent-id", "x-claude-code-parent-agent-id", "x-claude-code-thread-id")
	if thread == "" {
		thread = strings.TrimSpace(body.Metadata.ParentID)
	}
	if thread == "" {
		thread = strings.TrimSpace(identity.AgentID)
	}
	if thread == "" {
		thread = strings.TrimSpace(identity.ParentSessionID)
	}
	return claudeRoute{thread: thread, session: session}
}

func claudeBodyForAccount(body []byte, account *Account, route claudeRoute) []byte {
	if account == nil || !strings.EqualFold(account.claims().Provider, "claude") {
		return body
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	var metadata map[string]json.RawMessage
	if raw := payload["metadata"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &metadata)
	}
	if metadata == nil {
		metadata = make(map[string]json.RawMessage)
	}
	identity := claudeIdentityFromMetadata(metadata["user_id"])
	claims := account.claims()
	identityRaw := map[string]string{
		"account_uuid": claims.AccountID,
		"device_id":    claims.DeviceID,
	}
	if identity.SessionID != "" {
		identityRaw["session_id"] = identity.SessionID
	}
	if identity.ParentSessionID != "" {
		identityRaw["parent_session_id"] = identity.ParentSessionID
	}
	if route.session != "" && identityRaw["session_id"] == "" {
		identityRaw["session_id"] = route.session
	}
	if route.thread != "" && identityRaw["parent_session_id"] == "" {
		identityRaw["parent_session_id"] = route.thread
	}
	encoded, err := json.Marshal(identityRaw)
	if err != nil {
		return body
	}
	userID, err := json.Marshal(string(encoded))
	if err != nil {
		return body
	}
	metadata["user_id"] = userID
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return body
	}
	payload["metadata"] = metadataBytes
	result, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return result
}

// claudeMessagesHTTP forwards Anthropic Messages requests using pooled OAuth
// accounts. It retries only before any upstream response bytes are sent and
// only for statuses that are safe to replay (rate limits and transient 5xx).
func (s *server) claudeMessagesHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeAPIKey(r); !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "use POST for Claude Messages")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, claudeMaxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if len(body) > claudeMaxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var parsed claudeMessageRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if strings.TrimSpace(parsed.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	route := claudeRouteFrom(r, parsed)
	owners, err := s.claudeRouteOwners(route, parsed.Model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load session route")
		return
	}
	if s.pool == nil {
		writeError(w, http.StatusServiceUnavailable, "no account pool configured")
		return
	}
	ctx := r.Context()
	if s.ctx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
	}
	skip := map[string]bool{}
	refreshed := map[string]bool{}
	for attempt := 0; attempt <= claudeMaxFailoverAttempt; attempt++ {
		account := s.selectClaudeAccountForModel(owners, skip, parsed.Model)
		if account == nil {
			writeError(w, http.StatusServiceUnavailable, "no Claude account available")
			return
		}
		response, requestErr := s.doClaudeMessages(ctx, r, body, account)
		if requestErr != nil {
			skip[claudeAccountID(account)] = true
			if attempt < claudeMaxFailoverAttempt {
				continue
			}
			writeError(w, http.StatusBadGateway, "Claude upstream unavailable")
			return
		}
		if response.StatusCode == http.StatusUnauthorized {
			id := claudeAccountID(account)
			if !refreshed[id] {
				client := s.client
				if client == nil {
					client = http.DefaultClient
				}
				var persist func(accountState) (accountState, error)
				if s.pool.store != nil {
					persist = s.pool.persistAccountState
				}
				if account.refresh(ctx, client, persist) == nil {
					response.Body.Close()
					refreshed[id] = true
					attempt--
					continue
				}
				refreshed[id] = true
			}
			if attempt < claudeMaxFailoverAttempt {
				response.Body.Close()
				skip[id] = true
				continue
			}
		}
		if claudeSafeFailoverStatus(response.StatusCode) && attempt < claudeMaxFailoverAttempt {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, claudeMaxResponseBody+1))
			response.Body.Close()
			if readErr == nil && len(data) <= claudeMaxResponseBody && claudeShouldFailover(response.StatusCode, response.Header, data) {
				if claudeCredentialRateLimited(response.Header) {
					account.rateLimited(response.Header)
				}
				skip[claudeAccountID(account)] = true
				continue
			}
			response.Body = io.NopCloser(bytes.NewReader(data))
		}
		defer response.Body.Close()
		account.observeClaudeHeaders(response.Header)
		copyClaudeResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			s.recordClaudeRoute(route, parsed.Model, account)
		}
		_, _ = io.CopyN(w, response.Body, claudeMaxResponseBody)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if response.StatusCode == http.StatusTooManyRequests {
				if claudeCredentialRateLimited(response.Header) {
					account.rateLimited(response.Header)
				} else {
					account.coolModel(parsed.Model, response.Header)
				}
			}
		}
		return
	}
}

func (s *server) claudeModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeAPIKey(r); !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if s.pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	endpoint, err := claudeModelsURL(s.upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "invalid Claude upstream URL")
		return
	}
	client := s.client
	if client == nil {
		client = newProxyClient()
	}
	skip := map[string]bool{}
	refreshed := map[string]bool{}
	for attempt := 0; attempt <= claudeMaxFailoverAttempt; attempt++ {
		account := s.selectClaudeAccountForModel(nil, skip, "")
		if account == nil {
			writeError(w, http.StatusServiceUnavailable, "no Claude account available")
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
		if err != nil {
			writeError(w, http.StatusBadGateway, "could not create Claude models request")
			return
		}
		copyClaudeRequestHeaders(request.Header, r.Header)
		setClaudeUpstreamHeaders(request.Header, account.persisted().AccessToken)
		response, err := client.Do(request)
		if err != nil {
			writeError(w, http.StatusBadGateway, "Claude models request failed")
			return
		}
		id := claudeAccountID(account)
		if response.StatusCode == http.StatusUnauthorized && !refreshed[id] {
			var persist func(accountState) (accountState, error)
			if s.pool.store != nil {
				persist = s.pool.persistAccountState
			}
			if account.refresh(r.Context(), client, persist) == nil {
				response.Body.Close()
				refreshed[id] = true
				attempt--
				continue
			}
			refreshed[id] = true
		}
		if response.StatusCode == http.StatusUnauthorized && attempt < claudeMaxFailoverAttempt {
			response.Body.Close()
			skip[id] = true
			continue
		}
		defer response.Body.Close()
		copyClaudeResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = io.CopyN(w, response.Body, claudeMaxResponseBody)
		return
	}
}

func claudeModelsURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid Claude upstream URL")
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/v1/messages"):
		u.Path = strings.TrimSuffix(path, "/messages") + "/models"
	case strings.HasSuffix(path, "/messages"):
		u.Path = strings.TrimSuffix(path, "/messages") + "/models"
	case strings.HasSuffix(path, "/v1"):
		u.Path = path + "/models"
	default:
		u.Path = path + "/v1/models"
	}
	return u.String(), nil
}

func (s *server) claudeRouteOwners(route claudeRoute, model string) ([]string, error) {
	if s.pool == nil || s.pool.store == nil {
		return nil, nil
	}
	owners := make([]string, 0, 2)
	if route.thread != "" {
		values, err := s.pool.store.routeOwnersFresh(claudeRouteKey(model, route.thread), "", time.Hour)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			owners = appendUniqueOwner(owners, value)
		}
	}
	if route.session != "" {
		values, err := s.pool.store.routeOwnersFresh("", claudeRouteKey(model, route.session), time.Hour)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			owners = appendUniqueOwner(owners, value)
		}
	}
	return owners, nil
}

func (s *server) selectClaudeAccount(owners []string, skip map[string]bool) *Account {
	return s.selectClaudeAccountForModel(owners, skip, "")
}

func (s *server) selectClaudeAccountForModel(owners []string, skip map[string]bool, model string) *Account {
	if s == nil || s.pool == nil {
		return nil
	}
	now := time.Now()
	accounts := s.pool.all()
	eligible := func(a *Account) bool {
		id := claudeAccountID(a)
		if id == "" || skip[id] {
			return false
		}
		if !strings.EqualFold(claimsFromToken(a.persisted().IDToken).Provider, "claude") {
			return false
		}
		candidate := a.routingCandidate()
		if model != "" && now.Before(a.modelCooldownUntil(model)) {
			return false
		}
		return !candidate.paused && candidate.reauth == "" && !now.Before(candidate.cooldown)
	}
	for _, owner := range owners {
		for _, account := range accounts {
			if claudeAccountID(account) == owner && eligible(account) {
				return account
			}
		}
	}
	var selected *Account
	selectedRank := 2
	for _, account := range accounts {
		if !eligible(account) {
			continue
		}
		candidate := account.routingCandidate()
		rank := 1
		if candidate.mode == routingModePriority {
			rank = 0
		}
		if selected == nil || rank < selectedRank || rank == selectedRank && candidate.lastUsed.Before(selected.routingCandidate().lastUsed) {
			selected = account
			selectedRank = rank
		}
	}
	return selected
}

func (s *server) doClaudeMessages(ctx context.Context, inbound *http.Request, body []byte, account *Account) (*http.Response, error) {
	upstream, err := claudeMessagesURLForPath(s.upstream, inbound.URL.Path)
	if err != nil {
		return nil, err
	}
	route := claudeRouteFrom(inbound, claudeRequestFromBody(body))
	body = claudeBodyForAccount(body, account, route)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyClaudeRequestHeaders(request.Header, inbound.Header)
	state := account.persisted()
	setClaudeUpstreamHeaders(request.Header, state.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	if route.session != "" && request.Header.Get("X-Claude-Code-Session-Id") == "" {
		request.Header.Set("X-Claude-Code-Session-Id", route.session)
	}
	if route.thread != "" && request.Header.Get("X-Claude-Code-Agent-Id") == "" {
		request.Header.Set("X-Claude-Code-Agent-Id", route.thread)
	}
	client := s.client
	if client == nil {
		client = newProxyClient()
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return copyClient.Do(request)
}

func claudeMessagesURL(base string) (string, error) {
	return claudeMessagesURLForPath(base, claudeMessagesPath)
}

func claudeMessagesURLForPath(base, requestPath string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid Claude upstream URL")
	}
	path := strings.TrimRight(u.Path, "/")
	suffix := "/messages"
	if strings.HasSuffix(requestPath, "/count_tokens") {
		suffix = "/messages/count_tokens"
	}
	switch {
	case strings.HasSuffix(path, "/v1/messages"), strings.HasSuffix(path, "/messages"):
		u.Path = strings.TrimSuffix(path, "/messages") + suffix
	case strings.HasSuffix(path, "/v1"):
		u.Path = path + suffix
	default:
		u.Path = path + "/v1" + suffix
	}
	return u.String(), nil
}

func setClaudeUpstreamHeaders(header http.Header, accessToken string) {
	header.Set("Authorization", "Bearer "+accessToken)
	if header.Get("Accept") == "" {
		header.Set("Accept", "application/json")
	}
	if header.Get("Anthropic-Version") == "" {
		header.Set("Anthropic-Version", "2023-06-01")
	}
	if header.Get("Anthropic-Beta") == "" {
		header.Set("Anthropic-Beta", "oauth-2025-04-20")
	}
	if header.Get("Anthropic-Dangerous-Direct-Browser-Access") == "" {
		header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	}
	if header.Get("X-App") == "" {
		header.Set("X-App", "cli")
	}
}

func copyClaudeRequestHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "x-api-key" || lower == "host" || lower == "content-length" || lower == "connection" {
			continue
		}
		if strings.HasPrefix(lower, "x-claude-") || strings.HasPrefix(lower, "anthropic-") || lower == "user-agent" || lower == "accept" {
			dst[name] = append([]string(nil), values...)
		}
	}
}

func copyClaudeResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower == "content-length" || lower == "transfer-encoding" || lower == "connection" {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

func claudeSafeFailoverStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, 529:
		return true
	default:
		return false
	}
}

func claudeShouldFailover(status int, headers http.Header, body []byte) bool {
	if status != http.StatusTooManyRequests {
		return true
	}
	if claudeCredentialRateLimited(headers) {
		return true
	}
	text := strings.ToLower(string(body))
	for _, phrase := range []string{
		"fast request rejected", "usage credits are required", "credits are required",
		"model overloaded", "model is overloaded", "model rate limit", "model quota",
		"overage", "fable",
	} {
		if strings.Contains(text, phrase) {
			return false
		}
	}
	return true
}

func claudeCredentialRateLimited(headers http.Header) bool {
	for _, name := range []string{
		"anthropic-ratelimit-unified-5h-status",
		"anthropic-ratelimit-unified-7d-status",
	} {
		status := strings.ToLower(strings.TrimSpace(headers.Get(name)))
		if status != "" && status != "accepted" && status != "ok" {
			return true
		}
	}
	return false
}

func (s *server) recordClaudeRoute(route claudeRoute, model string, account *Account) {
	if account == nil || s.pool == nil || s.pool.store == nil || route.thread == "" && route.session == "" {
		return
	}
	at := time.Now()
	_ = s.pool.store.recordRoute(storedRoute{
		At: at, Thread: claudeRouteKey(model, route.thread), Session: claudeRouteKey(model, route.session), Account: claudeAccountID(account),
	})
	account.accepted(at)
}

func claudeAccountID(account *Account) string {
	if account == nil {
		return ""
	}
	if id := account.id(); id != "" {
		return id
	}
	state := account.persisted()
	for _, token := range []string{state.IDToken, state.AccessToken, state.RefreshToken} {
		if token == "" {
			continue
		}
		sum := sha256.Sum256([]byte(token))
		return fmt.Sprintf("claude-%x", sum[:8])
	}
	return ""
}

func (a *Account) observeClaudeHeaders(h http.Header) {
	for _, prefix := range []string{
		"anthropic-ratelimit-unified-5h", "anthropic-ratelimit-unified-7d",
		"x-ratelimit-5h", "x-ratelimit-7d",
	} {
		utilization := strings.TrimSpace(h.Get(prefix + "-utilization"))
		if utilization == "" {
			continue
		}
		value, err := strconv.ParseFloat(utilization, 64)
		if err != nil || value < 1 {
			continue
		}
		for _, name := range []string{prefix + "-reset", prefix + "-reset-at"} {
			if reset := parseClaudeReset(h.Get(name)); !reset.IsZero() {
				a.extendCooldown(reset)
			}
		}
	}
}

func (a *Account) modelCooldownUntil(model string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.modelCooldown[strings.TrimSpace(model)]
}

func (a *Account) coolModel(model string, h http.Header) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	until := time.Now().Add(minCooldown)
	if retryAfter := retryAfterHeader(h); retryAfter.After(until) {
		until = retryAfter
	}
	limit := time.Now().Add(maxCooldown)
	if until.After(limit) {
		until = limit
	}
	a.mu.Lock()
	if a.modelCooldown == nil {
		a.modelCooldown = make(map[string]time.Time)
	}
	if until.After(a.modelCooldown[model]) {
		a.modelCooldown[model] = until
	}
	a.mu.Unlock()
}

func parseClaudeReset(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs < 1e10 {
			return time.Unix(secs, 0)
		}
		return time.UnixMilli(secs)
	}
	if date, err := http.ParseTime(value); err == nil {
		return date
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return time.Now().Add(duration)
	}
	return time.Time{}
}

func claudeMediaType(h http.Header) string {
	mediaType, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	return mediaType
}

func (s *server) claudeMessagesMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodPost)
	writeError(w, http.StatusMethodNotAllowed, "use POST for Claude Messages")
}
