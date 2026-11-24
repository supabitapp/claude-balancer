package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type claudeUpstreamCall struct {
	token string
	body  string
	beta  string
}

type claudeFakeUpstream struct {
	mu      sync.Mutex
	calls   []claudeUpstreamCall
	respond func(call claudeUpstreamCall, index int, w http.ResponseWriter)
}

func newClaudeFakeUpstream(t *testing.T, respond func(call claudeUpstreamCall, index int, w http.ResponseWriter)) (*claudeFakeUpstream, string) {
	t.Helper()
	fake := &claudeFakeUpstream{respond: respond}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := claudeUpstreamCall{
			token: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			body:  string(body),
			beta:  r.Header.Get("Anthropic-Beta"),
		}
		fake.mu.Lock()
		index := len(fake.calls)
		fake.calls = append(fake.calls, call)
		fake.mu.Unlock()
		fake.respond(call, index, w)
	}))
	t.Cleanup(server.Close)
	return fake, server.URL
}

func (f *claudeFakeUpstream) snapshot() []claudeUpstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]claudeUpstreamCall(nil), f.calls...)
}

func sendClaudeMessage(t *testing.T, srv *server, session, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test")
	request.Header.Set("X-Claude-Code-Session-Id", session)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	srv.claudeMessagesHTTP(response, request)
	return response
}

func writeClaudeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
}

func TestClaudeFailoverHandsRouteToTheAccountThatServed(t *testing.T) {
	a, b := testAccount("account-a", 10), testAccount("account-b", 10)
	fake, upstream := newClaudeFakeUpstream(t, func(_ claudeUpstreamCall, index int, w http.ResponseWriter) {
		if index == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeClaudeOK(w)
	})
	srv := newTestServer(t, []*Account{a, b})
	srv.upstream = upstream
	srv.lookupAPIKey = func(string) (string, bool, error) { return "test", true, nil }

	response := sendClaudeMessage(t, srv, "session-1", `{"model":"claude-sonnet","messages":[]}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	calls := fake.snapshot()
	if len(calls) != 2 || calls[0].token == calls[1].token {
		t.Fatalf("calls = %+v, want one failover to a second account", calls)
	}
	served := strings.TrimPrefix(calls[1].token, "token-")
	owners, err := srv.claudeRouteOwners(claudeRoute{session: "session-1"}, "claude-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || owners[0] != served {
		t.Fatalf("route owners = %v, want [%s], the account that served the turn", owners, served)
	}
}

func TestFailedRetryKeepsARouteAnotherRequestWon(t *testing.T) {
	accounts := []*Account{testAccount("account-a", 10), testAccount("account-b", 10), testAccount("account-c", 10)}
	var srv *server
	winner := ""
	_, upstream := newClaudeFakeUpstream(t, func(call claudeUpstreamCall, index int, w http.ResponseWriter) {
		if index == 0 {
			winner = strings.TrimPrefix(call.token, "token-")
			for _, account := range accounts {
				if claudeAccountID(account) == winner {
					srv.recordClaudeRoute(claudeRoute{session: "session-1"}, "claude-sonnet", account, []string{winner})
				}
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv = newTestServer(t, accounts)
	srv.upstream = upstream
	srv.lookupAPIKey = func(string) (string, bool, error) { return "test", true, nil }

	if response := sendClaudeMessage(t, srv, "session-1", `{"model":"claude-sonnet","messages":[]}`, nil); response.Code == http.StatusOK {
		t.Fatal("every account failed, want an error")
	}
	owners, err := srv.claudeRouteOwners(claudeRoute{session: "session-1"}, "claude-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || owners[0] != winner {
		t.Fatalf("route owners = %v, want [%s] from the request that succeeded", owners, winner)
	}
}
