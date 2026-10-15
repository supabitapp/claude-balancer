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
		UserID    string `json:"user_id"`
		ParentID  string `json:"parent_id"`
		SessionID string `json:"session_id"`
	} `json:"metadata"`
}

type claudeRoute struct {
	thread  string
	session string
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
	session := firstWebSocketHeader(r.Header,
		"x-claude-code-session-id", "x-session-id", "session-id", "session_id")
	if session == "" {
		session = strings.TrimSpace(body.Metadata.SessionID)
	}
	thread := firstWebSocketHeader(r.Header,
		"x-claude-code-agent-id", "x-claude-code-thread-id", "x-client-request-id")
	if thread == "" {
		thread = strings.TrimSpace(body.Metadata.ParentID)
	}
	return claudeRoute{thread: thread, session: session}
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
	route := claudeRouteFrom(r, parsed)
	owners, err := s.claudeRouteOwners(route)
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
	for attempt := 0; attempt <= claudeMaxFailoverAttempt; attempt++ {
		account := s.selectClaudeAccount(owners, skip)
		if account == nil {
			writeError(w, http.StatusServiceUnavailable, "no Claude account available")
			return
		}
		response, requestErr := s.doClaudeMessages(ctx, r, body, account)
		if requestErr != nil {
			account.failed()
			skip[claudeAccountID(account)] = true
			if attempt < claudeMaxFailoverAttempt {
				continue
			}
			writeError(w, http.StatusBadGateway, "Claude upstream unavailable")
			return
		}
		if claudeSafeFailoverStatus(response.StatusCode) && attempt < claudeMaxFailoverAttempt {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, claudeMaxResponseBody+1))
			response.Body.Close()
			if readErr == nil && len(data) <= claudeMaxResponseBody {
				account.observeClaudeHeaders(response.Header)
				if response.StatusCode == http.StatusTooManyRequests {
					account.rateLimited(response.Header)
				}
				skip[claudeAccountID(account)] = true
				continue
			}
		}
		defer response.Body.Close()
		account.observeClaudeHeaders(response.Header)
		copyClaudeResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = io.CopyN(w, response.Body, claudeMaxResponseBody+1)
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			s.recordClaudeRoute(route, account)
		} else {
			if response.StatusCode == http.StatusTooManyRequests {
				account.rateLimited(response.Header)
			}
		}
		return
	}
}

func (s *server) claudeRouteOwners(route claudeRoute) ([]string, error) {
	if s.pool == nil || s.pool.store == nil {
		return nil, nil
	}
	owners := make([]string, 0, 2)
	if route.thread != "" {
		values, err := s.pool.store.routeOwners(route.thread, "")
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			owners = appendUniqueOwner(owners, value)
		}
	}
	if route.session != "" {
		values, err := s.pool.store.routeOwners("", route.session)
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
		candidate := a.routingCandidate()
		return !candidate.paused && candidate.reauth == "" && !candidate.spent && !candidate.weeklyExhausted() && !now.Before(candidate.cooldown)
	}
	for _, owner := range owners {
		for _, account := range accounts {
			if claudeAccountID(account) == owner && eligible(account) {
				return account
			}
		}
	}
	var selected *Account
	for _, account := range accounts {
		if !eligible(account) {
			continue
		}
		if selected == nil || account.routingCandidate().lastUsed.Before(selected.routingCandidate().lastUsed) {
			selected = account
		}
	}
	return selected
}

func (s *server) doClaudeMessages(ctx context.Context, inbound *http.Request, body []byte, account *Account) (*http.Response, error) {
	upstream, err := claudeMessagesURL(s.upstream)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyClaudeRequestHeaders(request.Header, inbound.Header)
	state := account.persisted()
	request.Header.Set("Authorization", "Bearer "+state.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", inbound.Header.Get("Accept"))
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "application/json")
	}
	if request.Header.Get("Anthropic-Version") == "" {
		request.Header.Set("Anthropic-Version", "2023-06-01")
	}
	if request.Header.Get("Anthropic-Beta") == "" {
		request.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	}
	if request.Header.Get("Anthropic-Dangerous-Direct-Browser-Access") == "" {
		request.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	}
	if request.Header.Get("X-App") == "" {
		request.Header.Set("X-App", "cli")
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
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid Claude upstream URL")
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/v1/messages"), strings.HasSuffix(path, "/messages"):
		u.Path = path
	case strings.HasSuffix(path, "/v1"):
		u.Path = path + "/messages"
	default:
		u.Path = path + "/v1/messages"
	}
	return u.String(), nil
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
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (s *server) recordClaudeRoute(route claudeRoute, account *Account) {
	if account == nil || s.pool == nil || s.pool.store == nil || route.thread == "" && route.session == "" {
		return
	}
	at := time.Now()
	_ = s.pool.store.recordRoute(storedRoute{At: at, Thread: route.thread, Session: route.session, Account: claudeAccountID(account)})
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
