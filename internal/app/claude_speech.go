package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	claudeSpeechPath         = "/api/ws/speech_to_text/voice_stream"
	claudeSpeechDialTimeout  = 15 * time.Second
	claudeSpeechWriteTimeout = 10 * time.Second
	claudeSpeechIdleTimeout  = 30 * time.Second
	claudeSpeechMaxSession   = 6 * time.Minute
	claudeSpeechMaxMessage   = 1 << 20
	claudeSpeechMaxKeyterms  = 1024
	claudeSpeechMaxTerms     = 50
)

var (
	claudeSpeechQueryParams = []string{
		"encoding", "sample_rate", "channels", "endpointing_ms", "utterance_end_ms",
		"language", "use_conversation_engine", "forward_interims",
	}
	claudeSpeechForwardedHeaders = []string{"User-Agent", "X-App", "Anthropic-Client-Platform", "X-Config-Keyterms"}
)

func (s *server) claudeSpeechHTTP(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := s.authorizeAPIKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer key")
		return
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") {
		w.Header().Set("Upgrade", "websocket")
		writeError(w, http.StatusUpgradeRequired, "speech to text requires a WebSocket upgrade")
		return
	}
	if !validClaudeSpeechKeyterms(r.Header.Get("X-Config-Keyterms")) {
		writeError(w, http.StatusBadRequest, "x-config-keyterms must be at most 50 printable ASCII terms in 1024 characters")
		return
	}
	endpoint, err := claudeSpeechURL(s.upstream, r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadGateway, "invalid Claude upstream URL")
		return
	}
	upstream, account, rejection, err := s.dialClaudeSpeech(r.Context(), endpoint, r.Header)
	switch {
	case err != nil:
		s.log.Warn("speech upstream unavailable", "error", err)
		writeError(w, http.StatusBadGateway, "Claude speech upstream unavailable")
		return
	case rejection != nil:
		writeClaudeSpeechRejection(w, rejection)
		return
	case upstream == nil:
		writeError(w, http.StatusServiceUnavailable, "no Claude account available")
		return
	}
	client, err := websocket.Accept(w, r, nil)
	if err != nil {
		upstream.CloseNow()
		s.log.Warn("speech websocket upgrade failed", "error", err)
		return
	}
	client.SetReadLimit(claudeSpeechMaxMessage)
	upstream.SetReadLimit(claudeSpeechMaxMessage)

	id := claudeAccountID(account)
	if s.stats != nil {
		s.stats.websocketOpened(id)
		defer s.stats.websocketClosed(id)
	}
	var shutdown <-chan struct{}
	if s.admission != nil {
		shutdown = s.admission.drainStarted()
	}
	log := s.log.With("account", id, "api_key", apiKey.name)
	log.Info("speech session opened")
	started := time.Now()
	relay := &claudeSpeechRelay{
		client:      client,
		upstream:    upstream,
		idleTimeout: claudeSpeechIdleTimeout,
		maxSession:  claudeSpeechMaxSession,
	}
	relay.run(shutdown)
	log.Info("speech session closed",
		"closed_by", relay.closedBy,
		"close_status", int(relay.closeStatus),
		"duration_ms", time.Since(started).Milliseconds(),
		"client_bytes", relay.clientBytes.Load(),
		"upstream_bytes", relay.upstreamBytes.Load(),
	)
}

// dialClaudeSpeech opens the upstream socket before the client is upgraded, so
// a rejected handshake can move to another account or surface as plain HTTP.
func (s *server) dialClaudeSpeech(ctx context.Context, endpoint string, inbound http.Header) (*websocket.Conn, *Account, *http.Response, error) {
	skip := map[string]bool{}
	var rejection *http.Response
	for attempt := 0; attempt <= claudeMaxFailoverAttempt; attempt++ {
		account := s.selectClaudeAccount(nil, skip)
		if account == nil {
			break
		}
		id := claudeAccountID(account)
		skip[id] = true
		conn, response, err := s.dialClaudeSpeechAccount(ctx, endpoint, inbound, account)
		switch {
		case err == nil:
			return conn, account, nil, nil
		case response == nil || response.StatusCode < http.StatusBadRequest:
			return nil, nil, nil, err
		}
		rejection = response
		s.log.Warn("speech upstream rejected", "account", id, "attempt", attempt+1, "status", response.StatusCode)
		switch response.StatusCode {
		case http.StatusUnauthorized:
			continue
		case http.StatusTooManyRequests:
			if claudeCredentialRateLimited(response.Header) {
				account.rateLimitedClaude(response.Header)
			}
			continue
		}
		return nil, nil, rejection, nil
	}
	return nil, nil, rejection, nil
}

func (s *server) dialClaudeSpeechAccount(ctx context.Context, endpoint string, inbound http.Header, account *Account) (*websocket.Conn, *http.Response, error) {
	id := claudeAccountID(account)
	refreshed := account.refreshDue(time.Now())
	if refreshed {
		s.refreshedContext(ctx, account, id)
	}
	for {
		dialCtx, cancel := context.WithTimeout(ctx, claudeSpeechDialTimeout)
		conn, response, err := websocket.Dial(dialCtx, endpoint, &websocket.DialOptions{
			HTTPClient: s.client,
			HTTPHeader: claudeSpeechHeaders(inbound, account.persisted().AccessToken),
		})
		cancel()
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized || refreshed {
			return conn, response, err
		}
		refreshed = true
		if !s.refreshedContext(ctx, account, id) {
			return nil, response, err
		}
	}
}

func claudeSpeechURL(base string, query url.Values) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid Claude upstream URL")
	}
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/messages")
	u.Path = strings.TrimSuffix(path, "/v1") + claudeSpeechPath
	forwarded := url.Values{}
	for _, name := range claudeSpeechQueryParams {
		if query.Has(name) {
			forwarded.Set(name, query.Get(name))
		}
	}
	u.RawQuery = forwarded.Encode()
	return u.String(), nil
}

func claudeSpeechHeaders(inbound http.Header, accessToken string) http.Header {
	headers := http.Header{}
	for _, name := range claudeSpeechForwardedHeaders {
		if value := inbound.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	headers.Set("Authorization", "Bearer "+accessToken)
	return headers
}

func validClaudeSpeechKeyterms(value string) bool {
	if len(value) > claudeSpeechMaxKeyterms || strings.Count(value, ",")+1 > claudeSpeechMaxTerms {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func headerHasToken(headers http.Header, name, target string) bool {
	for _, value := range headers.Values(name) {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), target) {
				return true
			}
		}
	}
	return false
}

func writeClaudeSpeechRejection(w http.ResponseWriter, response *http.Response) {
	if retryAfter := response.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	writeError(w, response.StatusCode, "Claude speech upstream rejected the connection")
}

// claudeSpeechRelay copies frames unchanged in both directions. Only the side
// that reads a failure decides the close sent to the other peer, so a peer's
// own close code and reason reach the opposite side intact.
type claudeSpeechRelay struct {
	client        *websocket.Conn
	upstream      *websocket.Conn
	idleTimeout   time.Duration
	maxSession    time.Duration
	activity      atomic.Int64
	clientBytes   atomic.Int64
	upstreamBytes atomic.Int64
	finished      sync.Once
	closedBy      string
	closeStatus   websocket.StatusCode
}

func (r *claudeSpeechRelay) run(shutdown <-chan struct{}) {
	r.activity.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	go r.pump(r.client, r.upstream, &r.clientBytes, done, "client", websocket.StatusGoingAway, "client disconnected")
	go r.pump(r.upstream, r.client, &r.upstreamBytes, done, "upstream", websocket.StatusBadGateway, "upstream connection lost")
	session := time.NewTimer(r.maxSession)
	defer session.Stop()
	idle := time.NewTimer(r.idleTimeout)
	defer idle.Stop()
	for running := 2; running > 0; {
		select {
		case <-done:
			running--
		case <-session.C:
			r.finish("balancer", websocket.StatusPolicyViolation, "speech session time limit reached")
		case <-idle.C:
			if wait := r.idleTimeout - time.Since(time.Unix(0, r.activity.Load())); wait > 0 {
				idle.Reset(wait)
				continue
			}
			r.finish("balancer", websocket.StatusPolicyViolation, "speech session idle timeout")
		case <-shutdown:
			shutdown = nil
			r.finish("balancer", websocket.StatusServiceRestart, "balancer shutting down")
		}
	}
}

func (r *claudeSpeechRelay) pump(src, dst *websocket.Conn, transferred *atomic.Int64, done chan<- struct{}, side string, lost websocket.StatusCode, reason string) {
	defer func() { done <- struct{}{} }()
	for {
		kind, data, err := src.Read(context.Background())
		if err != nil {
			status, closeReason := claudeSpeechCloseStatus(err, lost, reason)
			r.finish(side, status, closeReason)
			return
		}
		r.activity.Store(time.Now().UnixNano())
		transferred.Add(int64(len(data)))
		ctx, cancel := context.WithTimeout(context.Background(), claudeSpeechWriteTimeout)
		err = dst.Write(ctx, kind, data)
		cancel()
		if err != nil {
			dst.CloseNow()
			return
		}
	}
}

func (r *claudeSpeechRelay) finish(closedBy string, status websocket.StatusCode, reason string) {
	r.finished.Do(func() {
		r.closedBy, r.closeStatus = closedBy, status
		var closing sync.WaitGroup
		closing.Go(func() { r.client.Close(status, reason) })
		closing.Go(func() { r.upstream.Close(status, reason) })
		closing.Wait()
	})
}

func claudeSpeechCloseStatus(err error, fallback websocket.StatusCode, reason string) (websocket.StatusCode, string) {
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return websocket.StatusMessageTooBig, "speech frame exceeded 1 MiB"
	}
	var closed websocket.CloseError
	if !errors.As(err, &closed) {
		return fallback, reason
	}
	if closed.Code == websocket.StatusNoStatusRcvd {
		return websocket.StatusNormalClosure, ""
	}
	return closed.Code, closed.Reason
}
