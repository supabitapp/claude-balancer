package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testSpeechQuery = "?encoding=linear16&sample_rate=16000&channels=1&endpointing_ms=300&utterance_end_ms=1000&language=en&use_conversation_engine=true"

type speechUpstreamLog struct {
	mu     sync.Mutex
	tokens []string
}

func (l *speechUpstreamLog) record(r *http.Request) string {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens = append(l.tokens, token)
	return token
}

func (l *speechUpstreamLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.tokens...)
}

func speechUpstream(t *testing.T, session func(*websocket.Conn, *http.Request)) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("upstream accept: %v", err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(claudeSpeechMaxMessage)
		session(conn, r)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func newSpeechBalancer(t *testing.T, upstream string, accounts ...*Account) string {
	t.Helper()
	srv := newTestServer(t, accounts)
	srv.upstream = upstream
	srv.lookupAPIKey = func(key string) (string, bool, error) { return "laptop", key == "balancer-key", nil }
	balancer := httptest.NewServer(srv.routes())
	t.Cleanup(balancer.Close)
	return "ws" + strings.TrimPrefix(balancer.URL, "http") + claudeSpeechPath + testSpeechQuery
}

func speechHeader() http.Header {
	return http.Header{"Authorization": {"Bearer balancer-key"}}
}

func dialSpeech(t *testing.T, endpoint string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: header})
	if conn != nil {
		t.Cleanup(func() { conn.CloseNow() })
	}
	return conn, response, err
}

func readSpeech(t *testing.T, conn *websocket.Conn) (websocket.MessageType, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return conn.Read(ctx)
}

func writeSpeech(t *testing.T, conn *websocket.Conn, kind websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, kind, data); err != nil {
		t.Fatal(err)
	}
}

func wantSpeechClose(t *testing.T, err error, status websocket.StatusCode, reason string) {
	t.Helper()
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != status || closed.Reason != reason {
		t.Fatalf("close = %v, want %d %q", err, status, reason)
	}
}

func receive[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for speech relay")
	}
	var zero T
	return zero
}

func TestClaudeSpeechRequiresAPIKey(t *testing.T) {
	var dialed atomic.Bool
	upstream := speechUpstream(t, func(*websocket.Conn, *http.Request) { dialed.Store(true) })
	endpoint := newSpeechBalancer(t, upstream.URL, testAccount("account-a", 0))

	for _, header := range []http.Header{nil, {"Authorization": {"Bearer wrong"}}} {
		_, response, err := dialSpeech(t, endpoint, header)
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("dial = %v, %v; want HTTP 401", response, err)
		}
	}
	if dialed.Load() {
		t.Fatal("upstream dialed without a valid balancer key")
	}
}

func TestClaudeSpeechRejectsInvalidRequestsBeforeDialing(t *testing.T) {
	var dialed atomic.Bool
	upstream := speechUpstream(t, func(*websocket.Conn, *http.Request) { dialed.Store(true) })
	endpoint := newSpeechBalancer(t, upstream.URL, testAccount("account-a", 0))

	request, err := http.NewRequest(http.MethodGet, "http"+strings.TrimPrefix(endpoint, "ws"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = speechHeader()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plain GET status = %d, want %d", response.StatusCode, http.StatusUpgradeRequired)
	}

	for name, keyterms := range map[string]string{
		"too long":  strings.Repeat("a", claudeSpeechMaxKeyterms+1),
		"too many":  strings.Repeat("a,", claudeSpeechMaxTerms) + "a",
		"non-ASCII": "Supaterm,Zürich",
	} {
		header := speechHeader()
		header.Set("X-Config-Keyterms", keyterms)
		_, response, err := dialSpeech(t, endpoint, header)
		if err == nil || response == nil || response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s keyterms dial = %v, %v; want HTTP 400", name, response, err)
		}
	}
	if dialed.Load() {
		t.Fatal("upstream dialed for an invalid request")
	}
}

func TestClaudeSpeechForwardsAllowlistedQueryAndHeaders(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := speechUpstream(t, func(conn *websocket.Conn, r *http.Request) {
		seen <- r
		conn.Close(websocket.StatusNormalClosure, "")
	})
	endpoint := newSpeechBalancer(t, upstream.URL, testAccount("account-a", 0))

	header := speechHeader()
	header.Set("User-Agent", "supaterm/0.0.0")
	header.Set("X-App", "supaterm")
	header.Set("Anthropic-Client-Platform", "desktop")
	header.Set("X-Config-Keyterms", "Supaterm,Tailscale")
	header.Set("X-Api-Key", "balancer-key")
	header.Set("Cookie", "session=secret")
	header.Set("X-Claude-Code-Session-Id", "session-id")
	conn, _, err := dialSpeech(t, endpoint+"&forward_interims=typed&debug=1&access_token=leak", header)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = readSpeech(t, conn)
	wantSpeechClose(t, err, websocket.StatusNormalClosure, "")

	request := receive(t, seen)
	if request.URL.Path != claudeSpeechPath {
		t.Fatalf("upstream path = %q", request.URL.Path)
	}
	want := map[string]string{
		"encoding": "linear16", "sample_rate": "16000", "channels": "1", "endpointing_ms": "300",
		"utterance_end_ms": "1000", "language": "en", "use_conversation_engine": "true", "forward_interims": "typed",
	}
	query := request.URL.Query()
	if len(query) != len(want) {
		t.Fatalf("upstream query = %v, want only %v", query, want)
	}
	for name, value := range want {
		if got := query.Get(name); got != value {
			t.Fatalf("query %s = %q, want %q", name, got, value)
		}
	}
	for name, value := range map[string]string{
		"Authorization":             "Bearer token-account-a",
		"User-Agent":                "supaterm/0.0.0",
		"X-App":                     "supaterm",
		"Anthropic-Client-Platform": "desktop",
		"X-Config-Keyterms":         "Supaterm,Tailscale",
	} {
		if got := request.Header.Get(name); got != value {
			t.Fatalf("upstream %s = %q, want %q", name, got, value)
		}
	}
	for _, name := range []string{"X-Api-Key", "Cookie", "X-Claude-Code-Session-Id"} {
		if got := request.Header.Get(name); got != "" {
			t.Fatalf("upstream %s = %q, want it stripped", name, got)
		}
	}
}

func TestClaudeSpeechRelaysFramesAndUpstreamClose(t *testing.T) {
	audio := bytes.Repeat([]byte{0x01, 0x80}, 48_000)
	received := make(chan [][]byte, 1)
	upstream := speechUpstream(t, func(conn *websocket.Conn, _ *http.Request) {
		ctx := context.Background()
		var frames [][]byte
		for range 3 {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				t.Errorf("upstream read: %v", err)
				return
			}
			frames = append(frames, append([]byte{byte(kind)}, data...))
		}
		received <- frames
		conn.Write(ctx, websocket.MessageText, []byte(`{"type":"TranscriptText","data":"hello Supaterm"}`))
		conn.Write(ctx, websocket.MessageText, []byte(`{"type":"TranscriptEndpoint"}`))
		conn.Write(ctx, websocket.MessageBinary, []byte{0xde, 0xad})
		conn.Close(4000, "stream finished")
	})
	endpoint := newSpeechBalancer(t, upstream.URL, testAccount("account-a", 0))
	conn, _, err := dialSpeech(t, endpoint, speechHeader())
	if err != nil {
		t.Fatal(err)
	}

	writeSpeech(t, conn, websocket.MessageBinary, audio)
	writeSpeech(t, conn, websocket.MessageText, []byte(`{"type":"KeepAlive"}`))
	writeSpeech(t, conn, websocket.MessageText, []byte(`{"type":"CloseStream"}`))
	frames := receive(t, received)
	want := [][]byte{
		append([]byte{byte(websocket.MessageBinary)}, audio...),
		append([]byte{byte(websocket.MessageText)}, `{"type":"KeepAlive"}`...),
		append([]byte{byte(websocket.MessageText)}, `{"type":"CloseStream"}`...),
	}
	for i := range want {
		if !bytes.Equal(frames[i], want[i]) {
			t.Fatalf("upstream frame %d differs (%d bytes, want %d)", i, len(frames[i]), len(want[i]))
		}
	}
	for _, expected := range []struct {
		kind websocket.MessageType
		data string
	}{
		{websocket.MessageText, `{"type":"TranscriptText","data":"hello Supaterm"}`},
		{websocket.MessageText, `{"type":"TranscriptEndpoint"}`},
		{websocket.MessageBinary, "\xde\xad"},
	} {
		kind, data, err := readSpeech(t, conn)
		if err != nil || kind != expected.kind || string(data) != expected.data {
			t.Fatalf("client read = %v %q %v, want %v %q", kind, data, err, expected.kind, expected.data)
		}
	}
	_, _, err = readSpeech(t, conn)
	wantSpeechClose(t, err, 4000, "stream finished")
}

func TestClaudeSpeechPropagatesClientClose(t *testing.T) {
	closed := make(chan error, 1)
	upstream := speechUpstream(t, func(conn *websocket.Conn, _ *http.Request) {
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				closed <- err
				return
			}
		}
	})
	endpoint := newSpeechBalancer(t, upstream.URL, testAccount("account-a", 0))
	conn, _, err := dialSpeech(t, endpoint, speechHeader())
	if err != nil {
		t.Fatal(err)
	}
	writeSpeech(t, conn, websocket.MessageText, []byte(`{"type":"KeepAlive"}`))
	if err := conn.Close(4001, "dictation resolved"); err != nil {
		t.Fatal(err)
	}
	wantSpeechClose(t, receive(t, closed), 4001, "dictation resolved")
}

func TestClaudeSpeechClosesOversizedClientFrames(t *testing.T) {
	closed := make(chan error, 1)
	upstream := speechUpstream(t, func(conn *websocket.Conn, _ *http.Request) {
		_, _, err := conn.Read(context.Background())
		closed <- err
	})
	endpoint := newSpeechBalancer(t, upstream.URL, testAccount("account-a", 0))
	conn, _, err := dialSpeech(t, endpoint, speechHeader())
	if err != nil {
		t.Fatal(err)
	}
	writeSpeech(t, conn, websocket.MessageBinary, make([]byte, claudeSpeechMaxMessage+1))
	_, _, err = readSpeech(t, conn)
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("client close = %v, want %d", err, websocket.StatusMessageTooBig)
	}
	wantSpeechClose(t, receive(t, closed), websocket.StatusGoingAway, "client disconnected")
}

func TestClaudeSpeechFailsOverRejectedUpgrades(t *testing.T) {
	refreshCalls := useOAuthRefreshServer(t)
	var log speechUpstreamLog
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch log.record(r) {
		case "token-account-a":
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			writeError(w, http.StatusTooManyRequests, "usage limit reached")
		case "token-account-b":
			writeError(w, http.StatusUnauthorized, "token expired")
		case "refreshed-token":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			conn.Close(websocket.StatusNormalClosure, "")
		}
	}))
	t.Cleanup(upstream.Close)
	accountA, accountB := testAccount("account-a", 0), testAccount("account-b", 0)
	endpoint := newSpeechBalancer(t, upstream.URL, accountA, accountB)

	conn, _, err := dialSpeech(t, endpoint, speechHeader())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = readSpeech(t, conn)
	wantSpeechClose(t, err, websocket.StatusNormalClosure, "")
	if got, want := strings.Join(log.seen(), ","), "token-account-a,token-account-b,refreshed-token"; got != want {
		t.Fatalf("upstream tokens = %s, want %s", got, want)
	}
	if refreshCalls() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls())
	}
	if _, _, cooldown, _ := accountA.health(); !cooldown.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("account-a cooldown = %v, want the usage-limit reset", cooldown)
	}
}

func TestClaudeSpeechFailsOverPersistentUnauthorized(t *testing.T) {
	refreshCalls := useOAuthRefreshServer(t)
	var log speechUpstreamLog
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if log.record(r) != "token-account-b" {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(upstream.Close)
	endpoint := newSpeechBalancer(t, upstream.URL, testAccount("account-a", 0), testAccount("account-b", 0))

	conn, _, err := dialSpeech(t, endpoint, speechHeader())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = readSpeech(t, conn)
	wantSpeechClose(t, err, websocket.StatusNormalClosure, "")
	if got, want := strings.Join(log.seen(), ","), "token-account-a,refreshed-token,token-account-b"; got != want {
		t.Fatalf("upstream tokens = %s, want %s", got, want)
	}
	if refreshCalls() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls())
	}
}

func TestClaudeSpeechRefreshesNearExpiryBeforeDialing(t *testing.T) {
	refreshCalls := useOAuthRefreshServer(t)
	var log speechUpstreamLog
	upstream := speechUpstream(t, func(conn *websocket.Conn, r *http.Request) {
		log.record(r)
		conn.Close(websocket.StatusNormalClosure, "")
	})
	account := testAccount("account-a", 0)
	account.LastRefresh = time.Now().Add(-tokenRefreshFallback - time.Hour)
	endpoint := newSpeechBalancer(t, upstream.URL, account)

	conn, _, err := dialSpeech(t, endpoint, speechHeader())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = readSpeech(t, conn)
	wantSpeechClose(t, err, websocket.StatusNormalClosure, "")
	if got := strings.Join(log.seen(), ","); got != "refreshed-token" {
		t.Fatalf("upstream tokens = %s, want refreshed-token", got)
	}
	if refreshCalls() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls())
	}
}

func TestClaudeSpeechReturnsFinalUpgradeRejection(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		attempts int
	}{
		{"usage limit on every account", http.StatusTooManyRequests, 2},
		{"server failure", http.StatusServiceUnavailable, 1},
		{"forbidden", http.StatusForbidden, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var log speechUpstreamLog
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record(r)
				w.Header().Set("Retry-After", "30")
				writeError(w, test.status, "speech unavailable")
			}))
			t.Cleanup(upstream.Close)
			endpoint := newSpeechBalancer(t, upstream.URL, testAccount("account-a", 0), testAccount("account-b", 0))

			_, response, err := dialSpeech(t, endpoint, speechHeader())
			if err == nil || response == nil || response.StatusCode != test.status {
				t.Fatalf("dial = %v, %v; want HTTP %d", response, err, test.status)
			}
			body, _ := io.ReadAll(response.Body)
			if !strings.Contains(string(body), "speech unavailable") || response.Header.Get("Retry-After") != "30" {
				t.Fatalf("rejection body = %s, Retry-After = %q", body, response.Header.Get("Retry-After"))
			}
			if got := len(log.seen()); got != test.attempts {
				t.Fatalf("upstream attempts = %d, want %d", got, test.attempts)
			}
		})
	}
}

func TestClaudeSpeechWithoutAccountsReturnsUnavailable(t *testing.T) {
	var dialed atomic.Bool
	upstream := speechUpstream(t, func(*websocket.Conn, *http.Request) { dialed.Store(true) })
	endpoint := newSpeechBalancer(t, upstream.URL)
	_, response, err := dialSpeech(t, endpoint, speechHeader())
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial = %v, %v; want HTTP 503", response, err)
	}
	if dialed.Load() {
		t.Fatal("upstream dialed without an account")
	}
}

func websocketPair(t *testing.T) (accepted, dialed *websocket.Conn) {
	t.Helper()
	conns := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		conns <- conn
	}))
	t.Cleanup(server.Close)
	dialed, _, err := websocket.Dial(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted = receive(t, conns)
	t.Cleanup(func() {
		accepted.CloseNow()
		dialed.CloseNow()
	})
	return accepted, dialed
}

func TestClaudeSpeechRelayEnforcesSessionLimits(t *testing.T) {
	stopped := make(chan struct{})
	close(stopped)
	for _, test := range []struct {
		name       string
		idle       time.Duration
		session    time.Duration
		shutdown   <-chan struct{}
		wantStatus websocket.StatusCode
		wantReason string
	}{
		{"idle", 50 * time.Millisecond, time.Minute, nil, websocket.StatusPolicyViolation, "speech session idle timeout"},
		{"session", time.Minute, 50 * time.Millisecond, nil, websocket.StatusPolicyViolation, "speech session time limit reached"},
		{"shutdown", time.Minute, time.Minute, stopped, websocket.StatusServiceRestart, "balancer shutting down"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, clientPeer := websocketPair(t)
			upstreamPeer, upstream := websocketPair(t)
			relay := &claudeSpeechRelay{client: client, upstream: upstream, idleTimeout: test.idle, maxSession: test.session}
			finished := make(chan struct{})
			go func() {
				relay.run(test.shutdown)
				close(finished)
			}()
			errs := make(chan error, 2)
			for _, peer := range []*websocket.Conn{clientPeer, upstreamPeer} {
				go func() {
					_, _, err := peer.Read(context.Background())
					errs <- err
				}()
			}
			for range 2 {
				wantSpeechClose(t, receive(t, errs), test.wantStatus, test.wantReason)
			}
			receive(t, finished)
			if relay.closedBy != "balancer" || relay.closeStatus != test.wantStatus {
				t.Fatalf("relay closed by %q with %d", relay.closedBy, relay.closeStatus)
			}
		})
	}
}
