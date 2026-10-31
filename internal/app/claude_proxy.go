package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	claudeMessagesPath       = "/v1/messages"
	claudeMessagesAliasPath  = "/messages"
	claudeMaxRequestBody     = 32 << 20
	claudeMaxResponseBody    = 256 << 20
	claudeUpstreamTimeout    = 90 * time.Second
	claudeMaxFailoverAttempt = 2
	claudeRouteFreshness     = time.Hour
	claudeOverloadedStatus   = 529
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
	ParentAgentID   string `json:"parent_agent_id"`
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
	if json.Unmarshal(raw, &identity) == nil && (identity.SessionID != "" || identity.AgentID != "" || identity.ParentSessionID != "" || identity.ParentAgentID != "") {
		return identity
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		if json.Unmarshal([]byte(encoded), &identity) == nil && (identity.SessionID != "" || identity.AgentID != "" || identity.ParentSessionID != "" || identity.ParentAgentID != "") {
			return identity
		}
		if index := strings.LastIndex(encoded, "_session_"); index >= 0 {
			identity.SessionID = normalizeClaudeID(encoded[index+len("_session_"):])
		}
	}
	return identity
}

type claudeRoute struct {
	thread        string
	session       string
	parentSession string
	agent         string
	parentAgent   string
}

func claudeRouteKey(model, value string) string {
	model = canonicalClaudeModel(model)
	value = normalizeClaudeID(value)
	if model == "" || value == "" {
		return ""
	}
	return "claude:" + model + ":" + value
}

func (r claudeRoute) identities() []string {
	keys := make([]string, 0, 5)
	appendKey := func(value string) {
		value = normalizeClaudeID(value)
		if value == "" {
			return
		}
		for _, existing := range keys {
			if existing == value {
				return
			}
		}
		keys = append(keys, value)
	}
	if r.session != "" {
		if r.agent != "" && !strings.EqualFold(r.agent, "main") {
			appendKey(claudeAgentIdentity(r.session, r.agent))
		}
		if r.parentAgent != "" && !strings.EqualFold(r.parentAgent, "main") {
			appendKey(claudeAgentIdentity(r.session, r.parentAgent))
		}
		if r.parentSession != "" {
			appendKey(normalizeClaudeID(r.parentSession))
		}
		appendKey(normalizeClaudeID(r.session))
	}
	appendKey(r.thread)
	return keys
}

func claudeAgentIdentity(session, agent string) string {
	identity := normalizeClaudeID(session) + ":agent:" + normalizeClaudeID(agent)
	if len(identity) <= 256 {
		return identity
	}
	sum := sha256.Sum256([]byte(identity))
	return identity[:192] + ":" + fmt.Sprintf("%x", sum[:16])
}

func (r claudeRoute) keys(model string) []string {
	keys := make([]string, 0, 5)
	for _, identity := range r.identities() {
		if key := claudeRouteKey(model, identity); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func (r claudeRoute) writeKeys(model string) []string {
	if r.session != "" {
		identity := r.session
		if r.agent != "" && !strings.EqualFold(r.agent, "main") {
			identity = claudeAgentIdentity(r.session, r.agent)
		}
		if key := claudeRouteKey(model, identity); key != "" {
			return []string{key}
		}
	}
	if key := claudeRouteKey(model, r.thread); key != "" {
		return []string{key}
	}
	return nil
}

func claudeRouteFrom(r *http.Request, body claudeMessageRequest) claudeRoute {
	identity := claudeIdentityFromMetadata(body.Metadata.UserID)
	session := normalizeClaudeID(r.Header.Get("X-Claude-Code-Session-Id"))
	if session == "" {
		session = normalizeClaudeID(identity.SessionID)
	}
	if session == "" {
		session = normalizeClaudeID(body.Metadata.SessionID)
	}
	if session == "" {
		session = normalizeClaudeID(firstWebSocketHeader(r.Header, "x-session-id", "session-id", "session_id"))
	}
	agent := normalizeClaudeID(r.Header.Get("X-Claude-Code-Agent-Id"))
	if agent == "" {
		agent = normalizeClaudeID(identity.AgentID)
	}
	parentAgent := normalizeClaudeID(r.Header.Get("X-Claude-Code-Parent-Agent-Id"))
	if parentAgent == "" {
		parentAgent = normalizeClaudeID(identity.ParentAgentID)
	}
	thread := normalizeClaudeID(r.Header.Get("X-Claude-Code-Thread-Id"))
	if thread == "" {
		thread = normalizeClaudeID(body.Metadata.ParentID)
	}
	if thread == "" {
		thread = normalizeClaudeID(identity.ParentSessionID)
	}
	if thread == "" && session == "" {
		thread = agent
	}
	return claudeRoute{thread: thread, session: session, parentSession: normalizeClaudeID(identity.ParentSessionID), agent: agent, parentAgent: parentAgent}
}

func normalizeClaudeID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return ""
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ""
		}
	}
	return value
}

func canonicalClaudeModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if open := strings.LastIndexByte(model, '('); open >= 0 && strings.HasSuffix(model, ")") {
		model = strings.TrimSpace(model[:open])
	}
	return model
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
	if route.parentSession != "" {
		identityRaw["parent_session_id"] = route.parentSession
	} else if identity.ParentSessionID != "" {
		identityRaw["parent_session_id"] = identity.ParentSessionID
	}
	if route.session != "" {
		identityRaw["session_id"] = route.session
	}
	if route.parentSession == "" && route.thread != "" && identityRaw["parent_session_id"] == "" {
		identityRaw["parent_session_id"] = route.thread
	}
	if route.agent != "" {
		identityRaw["agent_id"] = route.agent
	}
	if route.parentAgent != "" {
		identityRaw["parent_agent_id"] = route.parentAgent
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
	apiKey, ok := s.authorizeAPIKey(r)
	if !ok {
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
	counted := s.stats != nil && !strings.HasSuffix(r.URL.Path, "/count_tokens")
	statsThread := route.statsThread(parsed.Model)
	metadata := route.turnMetadata()
	if counted {
		s.stats.activateThread(statsThread)
		defer s.stats.deactivateThread(statsThread)
	}
	skip := map[string]bool{}
	refreshed := map[string]bool{}
	var overloadHeader http.Header
	var overloadBody []byte
	for attempt := 0; attempt <= claudeMaxFailoverAttempt; attempt++ {
		account := s.selectClaudeAccountForModel(owners, skip, parsed.Model)
		if account == nil {
			if overloadBody != nil {
				writeClaudeOverload(w, overloadHeader, overloadBody)
				return
			}
			if retryAfter := s.claudeModelRetryAfter(parsed.Model); retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.FormatInt(int64((retryAfter+time.Second-1)/time.Second), 10))
				writeError(w, http.StatusTooManyRequests, "Claude model is temporarily unavailable")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "no Claude account available")
			return
		}
		s.reserveClaudeRoute(route, parsed.Model, account, owners)
		sent := time.Now()
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
		if parsed.Stream && response.StatusCode >= 200 && response.StatusCode < 300 {
			bootstrap, stream, streamErr := claudeStreamBootstrap(response.Body)
			if streamErr != nil {
				response.Body.Close()
				if ctx.Err() != nil {
					writeError(w, http.StatusBadGateway, "Claude stream canceled before output")
					return
				}
				if attempt < claudeMaxFailoverAttempt {
					skip[claudeAccountID(account)] = true
					continue
				}
				writeError(w, http.StatusBadGateway, "Claude stream ended before output")
				return
			}
			if first := claudeStreamFirstData(bootstrap); claudeOverloadError(first) {
				stream.Close()
				response.StatusCode = claudeOverloadedStatus
				response.Body = io.NopCloser(bytes.NewReader(first))
			} else {
				response.Body = &claudeStreamReadCloser{Reader: io.MultiReader(bytes.NewReader(bootstrap), stream), stream: stream}
			}
		}
		var failureBody []byte
		if claudeSafeFailoverStatus(response.StatusCode) && attempt < claudeMaxFailoverAttempt {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, claudeMaxResponseBody+1))
			response.Body.Close()
			scope := claude429Model
			if response.StatusCode == http.StatusTooManyRequests {
				scope = claude429ScopeFor(response.Header, data)
			}
			if readErr == nil && len(data) <= claudeMaxResponseBody && claudeShouldFailover(response.StatusCode, response.Header, data) {
				if claudeOverloaded(response.StatusCode, data) {
					overloadHeader, overloadBody = response.Header, claudeRetryableOverload(response.StatusCode, data)
				}
				if scope == claude429Credential {
					account.rateLimitedClaude(response.Header)
				} else if scope == claude429Model {
					account.coolModel(parsed.Model, response.Header)
				}
				skip[claudeAccountID(account)] = true
				continue
			}
			failureBody = data
			response.Body = io.NopCloser(bytes.NewReader(data))
		}
		failed := response.StatusCode < 200 || response.StatusCode >= 300
		if failed && failureBody == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, claudeMaxResponseBody+1))
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(data))
			if readErr == nil && len(data) <= claudeMaxResponseBody {
				failureBody = data
			}
		}
		if failed && claudeOverloaded(response.StatusCode, failureBody) {
			account.observeClaudeHeaders(response.Header)
			writeClaudeOverload(w, response.Header, claudeRetryableOverload(response.StatusCode, failureBody))
			return
		}
		defer response.Body.Close()
		account.observeClaudeHeaders(response.Header)
		copyClaudeResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if counted {
				accountID := claudeAccountID(account)
				s.stats.recordAccepted(time.Now(), statsThread, requestIP(r), apiKey, accountID, parsed.Model, "", "", transportHTTP, metadata, true)
				s.stats.answered(statsThread, accountID, time.Since(sent))
				var responseBody bytes.Buffer
				_, copyErr := io.Copy(io.MultiWriter(w, &responseBody), io.LimitReader(response.Body, claudeMaxResponseBody))
				if copyErr == nil {
					account.successfulClaudeModel(parsed.Model)
					s.recordClaudeRoute(route, parsed.Model, account, owners)
					s.stats.recordAPIKeyUsage(apiKey.name, statsThread, accountID, parsed.Model, "", "", claudeResponseUsage(responseBody.Bytes()))
					s.stats.completed(statsThread, accountID, metadata, time.Since(sent))
				}
				return
			}
			_, copyErr := io.Copy(w, io.LimitReader(response.Body, claudeMaxResponseBody))
			if copyErr == nil {
				account.successfulClaudeModel(parsed.Model)
				s.recordClaudeRoute(route, parsed.Model, account, owners)
			}
			return
		}
		_, _ = io.Copy(w, io.LimitReader(response.Body, claudeMaxResponseBody))
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if response.StatusCode == http.StatusTooManyRequests {
				scope := claude429ScopeFor(response.Header, failureBody)
				if scope == claude429Credential {
					account.rateLimitedClaude(response.Header)
				} else if scope == claude429Model {
					account.coolModel(parsed.Model, response.Header)
				}
			}
		}
		return
	}
}

type claudeStreamReadCloser struct {
	io.Reader
	stream io.Closer
}

func (r *claudeStreamReadCloser) Close() error {
	return r.stream.Close()
}

func claudeStreamBootstrap(body io.ReadCloser) ([]byte, io.ReadCloser, error) {
	const maxBootstrap = 64 << 10
	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for len(buffer) < maxBootstrap {
		n, err := body.Read(chunk)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			if claudeStreamFirstData(buffer) != nil {
				return buffer, body, nil
			}
		}
		if err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, errors.New("Claude stream bootstrap exceeded limit")
}

func claudeStreamFirstData(data []byte) []byte {
	for {
		boundary := bytes.Index(data, []byte("\n\n"))
		if boundary < 0 {
			return nil
		}
		event := data[:boundary]
		for _, line := range bytes.Split(event, []byte("\n")) {
			payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
			payload = bytes.TrimSpace(payload)
			if ok && len(payload) > 0 {
				return payload
			}
		}
		data = data[boundary+2:]
	}
}

func claudeOverloaded(status int, body []byte) bool {
	return status == claudeOverloadedStatus || claudeOverloadError(body)
}

func claudeOverloadError(body []byte) bool {
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.Error.Type == "overloaded_error"
}

func claudeRetryableOverload(status int, body []byte) []byte {
	envelope := map[string]json.RawMessage{}
	details := map[string]json.RawMessage{}
	if json.Unmarshal(body, &envelope) != nil || json.Unmarshal(envelope["error"], &details) != nil || details == nil {
		envelope = map[string]json.RawMessage{"type": json.RawMessage(`"error"`)}
		details = map[string]json.RawMessage{"message": json.RawMessage(`"Overloaded"`)}
	}
	if upstreamType, ok := details["type"]; ok {
		details["upstream_type"] = upstreamType
	}
	details["type"] = json.RawMessage(`"api_error"`)
	envelope["error"], _ = json.Marshal(details)
	envelope["upstream_status"] = json.RawMessage(strconv.Itoa(status))
	data, _ := json.Marshal(envelope)
	return data
}

func writeClaudeOverload(w http.ResponseWriter, upstream http.Header, body []byte) {
	copyClaudeResponseHeaders(w.Header(), upstream)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Should-Retry", "true")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}

func (s *server) reserveClaudeRoute(route claudeRoute, model string, account *Account, owners []string) {
	if account == nil || s.pool == nil || s.pool.store == nil {
		return
	}
	keys := route.writeKeys(model)
	if len(keys) == 0 {
		return
	}
	expected := ""
	if len(owners) > 0 {
		expected = owners[0]
	}
	now := time.Now()
	_ = s.pool.store.recordRouteIfOwner(storedRoute{At: now, Thread: keys[0], Aliases: keys[1:], Account: claudeAccountID(account)}, expected, now.Add(-claudeRouteFreshness))
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
	owners := make([]string, 0, 3)
	for _, key := range route.keys(model) {
		values, err := s.pool.store.routeOwnersFresh(key, "", claudeRouteFreshness)
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

func (s *server) claudeModelRetryAfter(model string) time.Duration {
	if s == nil || s.pool == nil {
		return 0
	}
	now := time.Now()
	var retryAt time.Time
	var eligible bool
	for _, account := range s.pool.all() {
		candidate := account.routingCandidate()
		if !strings.EqualFold(candidate.provider, "claude") || candidate.id == "" || candidate.paused || candidate.reauth != "" || now.Before(candidate.cooldown) {
			continue
		}
		eligible = true
		accountRetryAt := candidate.quotaRetryAt(model, now)
		cooldown := account.modelCooldownUntil(model)
		if cooldown.After(now) && (accountRetryAt.IsZero() || cooldown.Before(accountRetryAt)) {
			accountRetryAt = cooldown
		}
		if accountRetryAt.IsZero() {
			return 0
		}
		if retryAt.IsZero() || accountRetryAt.Before(retryAt) {
			retryAt = accountRetryAt
		}
	}
	if !eligible || retryAt.IsZero() {
		return 0
	}
	return time.Until(retryAt)
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
		if model != "" && (candidate.spent || now.Before(a.modelCooldownUntil(model)) || candidate.quotaExhausted(model, now)) {
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
	var candidates []*Account
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
		if rank < selectedRank {
			candidates = candidates[:0]
			selectedRank = rank
		}
		if rank == selectedRank {
			candidates = append(candidates, account)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return s.pool.selectClaudeRoundRobin(model, candidates)
}

func (s *server) doClaudeMessages(ctx context.Context, inbound *http.Request, body []byte, account *Account) (*http.Response, error) {
	upstream, err := claudeMessagesURLForPath(s.upstream, inbound.URL.Path)
	if err != nil {
		return nil, err
	}
	route := claudeRouteFrom(inbound, claudeRequestFromBody(body))
	if !strings.HasSuffix(inbound.URL.Path, "/count_tokens") {
		body = claudeBodyForAccount(body, account, route)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyClaudeRequestHeaders(request.Header, inbound.Header)
	state := account.persisted()
	setClaudeUpstreamHeaders(request.Header, state.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Del("X-Claude-Code-Session-Id")
	request.Header.Del("X-Claude-Code-Agent-Id")
	request.Header.Del("X-Claude-Code-Parent-Agent-Id")
	if route.session != "" {
		request.Header.Set("X-Claude-Code-Session-Id", route.session)
	}
	if route.agent != "" {
		request.Header.Set("X-Claude-Code-Agent-Id", route.agent)
	}
	if route.parentAgent != "" {
		request.Header.Set("X-Claude-Code-Parent-Agent-Id", route.parentAgent)
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
	betas := header.Values("Anthropic-Beta")
	joined := strings.Join(betas, ",")
	for _, beta := range []string{"oauth-2025-04-20", "claude-code-20250219"} {
		if !strings.Contains(strings.ToLower(joined), beta) {
			if joined != "" {
				joined += ","
			}
			joined += beta
		}
	}
	header.Set("Anthropic-Beta", joined)
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
	case http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, claudeOverloadedStatus:
		return true
	default:
		return false
	}
}

func claudeShouldFailover(status int, headers http.Header, body []byte) bool {
	return status != http.StatusTooManyRequests || claude429ScopeFor(headers, body) != claude429Request
}

func claudeCredentialRateLimited(headers http.Header) bool {
	return claude429ScopeFor(headers, nil) == claude429Credential
}

type claude429Scope uint8

const (
	claude429Model claude429Scope = iota
	claude429Credential
	claude429Request
)

func claudeHeaderValue(headers http.Header, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) || len(values) == 0 {
			continue
		}
		return strings.TrimSpace(values[0])
	}
	return ""
}

func claudeStatusHealthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "allowed", "allowed_warning", "accepted", "ok":
		return true
	default:
		return false
	}
}

func claudeStatusRejected(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "rejected")
}

func claudeSharedHealthy(headers http.Header, fiveHour, sevenDay string) bool {
	valid := 0
	for _, pair := range []struct {
		status string
		name   string
	}{
		{fiveHour, "anthropic-ratelimit-unified-5h-utilization"},
		{sevenDay, "anthropic-ratelimit-unified-7d-utilization"},
	} {
		if pair.status != "" {
			if !claudeStatusHealthy(pair.status) {
				return false
			}
			valid++
			continue
		}
		utilization := claudeHeaderValue(headers, pair.name)
		if utilization == "" {
			continue
		}
		value, err := strconv.ParseFloat(utilization, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= 1 {
			return false
		}
		valid++
	}
	return valid > 0
}

func claude429ScopeFor(headers http.Header, body []byte) claude429Scope {
	fiveHour := claudeHeaderValue(headers, "anthropic-ratelimit-unified-5h-status")
	sevenDay := claudeHeaderValue(headers, "anthropic-ratelimit-unified-7d-status")
	weeklyOI := claudeHeaderValue(headers, "anthropic-ratelimit-unified-7d_oi-status")
	overage := claudeHeaderValue(headers, "anthropic-ratelimit-unified-overage-status")
	unified := claudeHeaderValue(headers, "anthropic-ratelimit-unified-status")
	representative := strings.ToLower(claudeHeaderValue(headers, "anthropic-ratelimit-unified-representative-claim"))
	overageDisabled := strings.TrimSpace(claudeHeaderValue(headers, "anthropic-ratelimit-unified-overage-disabled-reason")) != ""
	headerSignal := fiveHour != "" || sevenDay != "" || weeklyOI != "" || overage != "" || unified != ""
	sharedHealthy := claudeSharedHealthy(headers, fiveHour, sevenDay)
	modelRejected := claudeStatusRejected(weeklyOI) || claudeStatusRejected(overage) || strings.Contains(representative, "overage") || overageDisabled
	if claudeStatusRejected(fiveHour) || claudeStatusRejected(sevenDay) {
		return claude429Credential
	}
	if modelRejected && sharedHealthy {
		return claude429Model
	}
	if claudeStatusRejected(unified) {
		if modelRejected && sharedHealthy {
			return claude429Model
		}
		return claude429Credential
	}
	if headerSignal {
		if modelRejected && sharedHealthy {
			return claude429Model
		}
		for _, status := range []string{fiveHour, sevenDay, weeklyOI, overage, unified} {
			if status != "" && !claudeStatusHealthy(status) {
				return claude429Credential
			}
		}
		return claude429Model
	}
	text := strings.ToLower(string(body))
	for _, phrase := range []string{"fast request rejected", "usage credits are required", "credits are required"} {
		if strings.Contains(text, phrase) {
			return claude429Request
		}
	}
	for _, phrase := range []string{"model overloaded", "model is overloaded", "model rate limit", "model quota"} {
		if strings.Contains(text, phrase) {
			return claude429Model
		}
	}
	return claude429Model
}

func (s *server) recordClaudeRoute(route claudeRoute, model string, account *Account, owners []string) {
	if account == nil || s.pool == nil || s.pool.store == nil || len(route.writeKeys(model)) == 0 {
		return
	}
	at := time.Now()
	keys := route.writeKeys(model)
	expected := ""
	if len(owners) > 0 {
		expected = owners[0]
	}
	_ = s.pool.store.recordRouteIfOwner(storedRoute{At: at, Thread: keys[0], Aliases: keys[1:], Account: claudeAccountID(account)}, expected, time.Time{})
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
	if claudeCredentialRateLimited(h) {
		a.rateLimitedClaude(h)
	}
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
	return a.modelCooldown[canonicalClaudeModel(model)]
}

func (a *Account) successfulClaudeModel(model string) {
	model = canonicalClaudeModel(model)
	a.mu.Lock()
	if a.modelCooldownAttempts != nil {
		delete(a.modelCooldownAttempts, model)
	}
	a.mu.Unlock()
}

func (a *Account) coolModel(model string, h http.Header) {
	model = canonicalClaudeModel(model)
	if model == "" {
		return
	}
	now := time.Now()
	a.mu.Lock()
	if a.modelCooldown == nil {
		a.modelCooldown = make(map[string]time.Time)
	}
	if a.modelCooldownAttempts == nil {
		a.modelCooldownAttempts = make(map[string]int)
	}
	a.modelCooldownAttempts[model]++
	attempt := a.modelCooldownAttempts[model]
	a.mu.Unlock()
	delay := minCooldown
	for index := 1; index < attempt && delay < 30*time.Minute; index++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	until := now.Add(delay)
	modelOnlyQuota := claudeHeaderValue(h, "anthropic-ratelimit-unified-7d_oi-status") != "" ||
		claudeHeaderValue(h, "anthropic-ratelimit-unified-overage-status") != "" ||
		claudeHeaderValue(h, "anthropic-ratelimit-unified-overage-disabled-reason") != ""
	if !modelOnlyQuota {
		if retryAfter := retryAfterHeader(h); retryAfter.After(until) {
			until = retryAfter
		}
	}
	a.mu.Lock()
	if until.After(a.modelCooldown[model]) {
		a.modelCooldown[model] = until
	}
	a.mu.Unlock()
}

func (a *Account) rateLimitedClaude(h http.Header) {
	now := time.Now()
	until := claudeRateLimitReset(h, now)
	if until.IsZero() {
		until = now.Add(minCooldown)
	}
	a.extendCooldown(until)
}

func claudeRateLimitReset(h http.Header, now time.Time) time.Time {
	scope := claude429ScopeFor(h, nil)
	if scope != claude429Credential {
		return time.Time{}
	}
	var latest time.Time
	consider := func(value string) {
		reset := parseClaudeReset(value)
		if reset.After(now) && (latest.IsZero() || reset.After(latest)) {
			latest = reset
		}
	}
	for _, prefix := range []string{"anthropic-ratelimit-unified-5h", "anthropic-ratelimit-unified-7d"} {
		utilization, err := strconv.ParseFloat(claudeHeaderValue(h, prefix+"-utilization"), 64)
		exhausted := err == nil && utilization >= 1 && !math.IsInf(utilization, 0)
		if !claudeStatusRejected(claudeHeaderValue(h, prefix+"-status")) && !exhausted {
			continue
		}
		consider(claudeHeaderValue(h, prefix+"-reset"))
		consider(claudeHeaderValue(h, prefix+"-reset-at"))
	}
	if latest.IsZero() {
		consider(claudeHeaderValue(h, "anthropic-ratelimit-unified-reset"))
	}
	if retryAfter := claudeRetryAfterHeader(h, now); retryAfter.After(now) && retryAfter.After(latest) {
		latest = retryAfter
	}
	max := now.Add(7*24*time.Hour + time.Hour)
	if latest.After(max) {
		return now.Add(minCooldown)
	}
	return latest
}

func claudeRetryAfterHeader(h http.Header, now time.Time) time.Time {
	value := claudeHeaderValue(h, "retry-after")
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return now.Add(time.Duration(min(seconds, int64(7*24*time.Hour/time.Second+time.Hour/time.Second))) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil {
		return date
	}
	return time.Time{}
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
