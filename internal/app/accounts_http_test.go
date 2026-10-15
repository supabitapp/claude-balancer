package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAccountLoginCallbackRemoteGET(t *testing.T) {
	srv := newTestServer(t, nil)
	flow := &claudeOAuthFlow{state: "state-value", callback: make(chan claudeOAuthCallback, 1)}
	srv.logins.active = &accountLogin{claudeFlow: flow, expiresAt: time.Now().Add(time.Minute)}

	req := httptest.NewRequest(http.MethodGet, "/callback?code=code-value&state=state-value", nil)
	response := httptest.NewRecorder()
	srv.accountLoginCallback(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	select {
	case result := <-flow.callback:
		if result.code != "code-value" || result.state != "state-value" {
			t.Fatalf("callback = %+v", result)
		}
	default:
		t.Fatal("callback was not delivered")
	}
}

func TestAccountLoginCallbackRejectsWrongState(t *testing.T) {
	srv := newTestServer(t, nil)
	flow := &claudeOAuthFlow{state: "state-value", callback: make(chan claudeOAuthCallback, 1)}
	srv.logins.active = &accountLogin{claudeFlow: flow, expiresAt: time.Now().Add(time.Minute)}

	req := httptest.NewRequest(http.MethodGet, "/callback?code=code-value&state=wrong-state", nil)
	response := httptest.NewRecorder()
	srv.accountLoginCallback(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	select {
	case result := <-flow.callback:
		t.Fatalf("wrong-state callback was delivered: %+v", result)
	default:
	}
}
